package shlink

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxPasscodeFailures is the default retry budget before passcode lockout.
	DefaultMaxPasscodeFailures = 5
)

var (
	ErrNotFound          = errors.New("shlink: link not found")
	ErrRevoked           = errors.New("shlink: link has been revoked")
	ErrExpired           = errors.New("shlink: link has expired")
	ErrMaxViewsReached   = errors.New("shlink: maximum allowed views reached")
	ErrRecipientRequired = errors.New("shlink: recipient parameter is required")
	ErrPasscodeRequired  = errors.New("shlink: valid passcode required")
	ErrPasscodeLockedOut = errors.New("shlink: link locked due to too many failed passcode attempts")
	ErrForbiddenPatient  = errors.New("shlink: link does not belong to target patient")
)

// ArtifactInput describes a plaintext clinical payload to encrypt into an SHLink.
type ArtifactInput struct {
	ContentType string // e.g. "application/smart-health-card" or "application/fhir+json"
	Plaintext   []byte
	Deflate     bool
}

// CreateRequest configures issuance of a new SMART Health Link.
type CreateRequest struct {
	Tenant              string
	PatientID           string
	BaseOrigin          string // e.g. "https://fhir.example.org"
	ViewerURL           string // optional viewer prefix before "#shlink:/..."
	Label               string
	Passcode            string
	ExpiresAt           time.Time // optional zero for non-expiring
	MaxViews            int       // optional 0 for unlimited
	MaxPasscodeFailures int       // optional 0 -> DefaultMaxPasscodeFailures
	LongTerm            bool      // adds "L" flag ("can-change" vs "finalized")
	Artifacts           []ArtifactInput
}

// IssuedLink contains the public metadata and client-only shlink:/ URI returned at issuance.
// Note: the 256-bit symmetric key is embedded ONLY in SHLinkURI and is never stored by Manager.
type IssuedLink struct {
	ID          string
	Tenant      string
	PatientID   string
	ManifestURL string
	SHLinkURI   string
	Flag        string
	Label       string
	Exp         int64
	QRCodeSVG   string
}

// EncryptedItem holds a pre-encrypted JWE artifact stored by the zero-knowledge SHLink server.
type EncryptedItem struct {
	ID          string
	ContentType string
	CompactJWE  string
}

type storedLink struct {
	ID                  string
	Tenant              string
	PatientID           string
	Label               string
	Flag                string
	LongTerm            bool
	ExpiresAt           time.Time
	PasscodeSalt        []byte
	PasscodeHash        [32]byte
	HasPasscode         bool
	FailedAttempts      int
	MaxPasscodeFailures int
	Views               int
	MaxViews            int
	Revoked             bool
	CreatedAt           time.Time
	Items               []EncryptedItem
}

// ManifestRequest represents the JSON body posted to POST /shlink/{id}/manifest.
type ManifestRequest struct {
	Recipient         string `json:"recipient"`
	Passcode          string `json:"passcode,omitempty"`
	EmbeddedLengthMax *int   `json:"embeddedLengthMax,omitempty"`
}

// ManifestFileEntry represents a single entry in the SHLink manifest response.
type ManifestFileEntry struct {
	ContentType string `json:"contentType"`
	Location    string `json:"location,omitempty"`
	Embedded    string `json:"embedded,omitempty"`
	LastUpdated string `json:"lastUpdated,omitempty"`
}

// ManifestResponse represents the SMART Health Links manifest JSON response.
type ManifestResponse struct {
	Status string              `json:"status,omitempty"`
	Files  []ManifestFileEntry `json:"files"`
}

// PasscodeError carries remaining retry count for 401 Unauthorized responses per SHLinks spec.
type PasscodeError struct {
	RemainingAttempts int
}

func (e *PasscodeError) Error() string {
	return ErrPasscodeRequired.Error()
}

func (e *PasscodeError) Unwrap() error {
	return ErrPasscodeRequired
}

// Manager manages SMART Health Links in a thread-safe zero-knowledge store
// (encrypting artifacts at issuance and never persisting the symmetric AES-256 key).
type Manager struct {
	mu    sync.RWMutex
	links map[string]*storedLink
	nowFn func() time.Time
}

// NewManager creates a new SMART Health Links Manager.
func NewManager() *Manager {
	return &Manager{
		links: make(map[string]*storedLink),
		nowFn: func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock used for expiration checks (for deterministic tests).
func (m *Manager) SetClock(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn != nil {
		m.nowFn = fn
	}
}

// Now returns the current UTC timestamp from the manager's configured clock.
func (m *Manager) Now() time.Time {
	m.mu.RLock()
	fn := m.nowFn
	m.mu.RUnlock()
	if fn != nil {
		return fn().UTC()
	}
	return time.Now().UTC()
}

func randomURLSafeID(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashPasscode(salt []byte, passcode string) [32]byte {
	h := sha256.New()
	_, _ = h.Write(salt)
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(passcode))
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// CreateLink encrypts the supplied clinical artifacts with a fresh 256-bit key,
// persists only the JWE ciphertexts + salted passcode hash, and returns the shlink:/ URI + SVG QR.
func (m *Manager) CreateLink(req CreateRequest) (*IssuedLink, error) {
	if len(req.Artifacts) == 0 {
		return nil, errors.New("shlink: at least one clinical artifact is required")
	}
	origin := strings.TrimRight(strings.TrimSpace(req.BaseOrigin), "/")
	if origin == "" {
		origin = "http://localhost:8080"
	}

	rawKey, keyB64, err := GenerateAES256Key()
	if err != nil {
		return nil, err
	}
	linkID, err := randomURLSafeID(24) // 192 bits of entropy per SHLinks >=128-bit requirement
	if err != nil {
		return nil, err
	}

	items := make([]EncryptedItem, 0, len(req.Artifacts))
	for i, art := range req.Artifacts {
		ct := strings.TrimSpace(art.ContentType)
		if ct == "" {
			ct = "application/fhir+json"
		}
		jwe, encErr := EncryptCompactJWE(rawKey, art.Plaintext, ct, art.Deflate)
		if encErr != nil {
			return nil, encErr
		}
		items = append(items, EncryptedItem{
			ID:          fmt.Sprintf("item-%d", i),
			ContentType: ct,
			CompactJWE:  jwe,
		})
	}
	// Zero raw symmetric key bytes after encrypting artifacts so key is never retained in memory.
	clear(rawKey)

	var flagBuilder strings.Builder
	if req.LongTerm {
		flagBuilder.WriteByte('L')
	}
	hasPasscode := strings.TrimSpace(req.Passcode) != ""
	var salt []byte
	var passHash [32]byte
	if hasPasscode {
		flagBuilder.WriteByte('P')
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		passHash = hashPasscode(salt, req.Passcode)
	}
	flagStr := flagBuilder.String()

	maxFailures := req.MaxPasscodeFailures
	if maxFailures <= 0 {
		maxFailures = DefaultMaxPasscodeFailures
	}

	manifestURL := origin + "/shlink/" + linkID + "/manifest"
	var expUnix int64
	if !req.ExpiresAt.IsZero() {
		expUnix = req.ExpiresAt.UTC().Unix()
	}

	payload := SHLinkPayload{
		URL:   manifestURL,
		Key:   keyB64,
		Exp:   expUnix,
		Flag:  flagStr,
		Label: strings.TrimSpace(req.Label),
		V:     1,
	}
	uri, err := EncodeSHLinkURI(payload, req.ViewerURL)
	if err != nil {
		return nil, err
	}
	qrSVG, err := RenderQRSVG(uri, 4)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	now := m.nowFn()
	m.links[linkID] = &storedLink{
		ID:                  linkID,
		Tenant:              strings.TrimSpace(req.Tenant),
		PatientID:           strings.TrimSpace(req.PatientID),
		Label:               payload.Label,
		Flag:                flagStr,
		LongTerm:            req.LongTerm,
		ExpiresAt:           req.ExpiresAt,
		PasscodeSalt:        salt,
		PasscodeHash:        passHash,
		HasPasscode:         hasPasscode,
		MaxPasscodeFailures: maxFailures,
		MaxViews:            req.MaxViews,
		CreatedAt:           now,
		Items:               items,
	}
	m.mu.Unlock()

	return &IssuedLink{
		ID:          linkID,
		Tenant:      strings.TrimSpace(req.Tenant),
		PatientID:   strings.TrimSpace(req.PatientID),
		ManifestURL: manifestURL,
		SHLinkURI:   uri,
		Flag:        flagStr,
		Label:       payload.Label,
		Exp:         expUnix,
		QRCodeSVG:   qrSVG,
	}, nil
}

// ResolveManifest validates recipient, revocation, expiration, passcode, and maxViews,
// returning the SHLink manifest and associated (tenant, patientID) for AuditEvent logging.
func (m *Manager) ResolveManifest(linkID string, baseOrigin string, req ManifestRequest) (*ManifestResponse, string, string, error) {
	if strings.TrimSpace(req.Recipient) == "" {
		metaTenant, metaPatient := m.LinkOwner(linkID)
		return nil, metaTenant, metaPatient, ErrRecipientRequired
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.links[strings.TrimSpace(linkID)]
	if !ok {
		return nil, "", "", ErrNotFound
	}
	if rec.Revoked {
		return nil, rec.Tenant, rec.PatientID, ErrRevoked
	}
	now := m.nowFn()
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		return nil, rec.Tenant, rec.PatientID, ErrExpired
	}
	if rec.HasPasscode {
		if rec.FailedAttempts >= rec.MaxPasscodeFailures {
			return nil, rec.Tenant, rec.PatientID, ErrPasscodeLockedOut
		}
		candidate := hashPasscode(rec.PasscodeSalt, req.Passcode)
		if req.Passcode == "" || subtle.ConstantTimeCompare(candidate[:], rec.PasscodeHash[:]) != 1 {
			rec.FailedAttempts++
			rem := max(rec.MaxPasscodeFailures-rec.FailedAttempts, 0)
			if rem == 0 {
				return nil, rec.Tenant, rec.PatientID, ErrPasscodeLockedOut
			}
			return nil, rec.Tenant, rec.PatientID, &PasscodeError{RemainingAttempts: rem}
		}
	}
	if rec.MaxViews > 0 && rec.Views >= rec.MaxViews {
		return nil, rec.Tenant, rec.PatientID, ErrMaxViewsReached
	}
	rec.Views++

	origin := strings.TrimRight(strings.TrimSpace(baseOrigin), "/")
	if origin == "" {
		origin = "http://localhost:8080"
	}
	status := "finalized"
	if rec.LongTerm {
		status = "can-change"
	}
	lastUpdated := rec.CreatedAt.UTC().Format(time.RFC3339)

	files := make([]ManifestFileEntry, 0, len(rec.Items))
	for _, it := range rec.Items {
		entry := ManifestFileEntry{
			ContentType: it.ContentType,
			Location:    origin + "/shlink/" + rec.ID + "/content/" + it.ID,
			LastUpdated: lastUpdated,
		}
		if req.EmbeddedLengthMax != nil && len(it.CompactJWE) <= *req.EmbeddedLengthMax {
			entry.Embedded = it.CompactJWE
		}
		files = append(files, entry)
	}
	return &ManifestResponse{
		Status: status,
		Files:  files,
	}, rec.Tenant, rec.PatientID, nil
}

// ReadContent returns the pre-encrypted compact JWE for a specific SHLink item,
// enforcing revocation and expiration checks.
func (m *Manager) ReadContent(linkID, itemID string) (compactJWE string, tenant string, patientID string, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, ok := m.links[strings.TrimSpace(linkID)]
	if !ok {
		return "", "", "", ErrNotFound
	}
	if rec.Revoked {
		return "", rec.Tenant, rec.PatientID, ErrRevoked
	}
	now := m.nowFn()
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		return "", rec.Tenant, rec.PatientID, ErrExpired
	}
	for _, it := range rec.Items {
		if it.ID == strings.TrimSpace(itemID) {
			return it.CompactJWE, rec.Tenant, rec.PatientID, nil
		}
	}
	return "", rec.Tenant, rec.PatientID, ErrNotFound
}

// Revoke permanently deactivates an issued SMART Health Link for the given tenant and patient.
func (m *Manager) Revoke(tenant, patientID, linkID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.links[strings.TrimSpace(linkID)]
	if !ok || (rec.Tenant != "" && tenant != "" && rec.Tenant != tenant) {
		return ErrNotFound
	}
	if patientID != "" && rec.PatientID != "" && rec.PatientID != patientID {
		return ErrForbiddenPatient
	}
	rec.Revoked = true
	rec.Items = nil
	return nil
}

// LinkOwner returns (tenant, patientID) for a link ID if known (used for audit enrichment).
func (m *Manager) LinkOwner(linkID string) (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.links[strings.TrimSpace(linkID)]
	if !ok {
		return "", ""
	}
	return rec.Tenant, rec.PatientID
}
