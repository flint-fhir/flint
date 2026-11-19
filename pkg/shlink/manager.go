package shlink

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
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
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	CompactJWE  string `json:"compactJwe"`
}

type storedLink struct {
	ID                  string          `json:"id"`
	Tenant              string          `json:"tenant"`
	PatientID           string          `json:"patientId"`
	Label               string          `json:"label"`
	Flag                string          `json:"flag"`
	LongTerm            bool            `json:"longTerm"`
	ExpiresAt           time.Time       `json:"expiresAt"`
	PasscodeSalt        []byte          `json:"passcodeSalt,omitempty"`
	PasscodeHash        [32]byte        `json:"passcodeHash"`
	HasPasscode         bool            `json:"hasPasscode"`
	FailedAttempts      int             `json:"failedAttempts"`
	MaxPasscodeFailures int             `json:"maxPasscodeFailures"`
	Views               int             `json:"views"`
	MaxViews            int             `json:"maxViews"`
	Revoked             bool            `json:"revoked"`
	CreatedAt           time.Time       `json:"createdAt"`
	Items               []EncryptedItem `json:"items,omitempty"`
}

// BackendStore is an optional driver-free persistence hook allowing stateless multi-replica
// clusters to share zero-knowledge SHLink ciphertexts, maxViews counters, and passcode lockouts
// with optimistic Compare-And-Swap concurrency control.
type BackendStore interface {
	CreateLink(id, tenant string, rawJSON []byte) error
	GetLink(id string) (rawJSON []byte, version int64, ok bool, err error)
	CompareAndSwapLink(id, tenant string, expectedVersion int64, rawJSON []byte) (bool, error)
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
	store BackendStore
	nowFn func() time.Time
}

// NewManager creates a new SMART Health Links Manager.
func NewManager() *Manager {
	return &Manager{
		links: make(map[string]*storedLink),
		nowFn: func() time.Time { return time.Now().UTC() },
	}
}

// SetBackendStore configures an optional cluster-shared BackendStore for SHLink records.
func (m *Manager) SetBackendStore(st BackendStore) {
	m.mu.Lock()
	m.store = st
	m.mu.Unlock()
}

func (m *Manager) getBackendStore() BackendStore {
	m.mu.RLock()
	st := m.store
	m.mu.RUnlock()
	return st
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

func (m *Manager) nowLocked() time.Time {
	if m.nowFn != nil {
		return m.nowFn().UTC()
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

	now := m.Now()
	rec := &storedLink{
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

	if st := m.getBackendStore(); st != nil {
		rawJSON, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		if err := st.CreateLink(linkID, rec.Tenant, rawJSON); err != nil {
			return nil, err
		}
	} else {
		m.mu.Lock()
		m.links[linkID] = rec
		m.mu.Unlock()
	}

	return &IssuedLink{
		ID:          linkID,
		Tenant:      rec.Tenant,
		PatientID:   rec.PatientID,
		ManifestURL: manifestURL,
		SHLinkURI:   uri,
		Flag:        flagStr,
		Label:       payload.Label,
		Exp:         expUnix,
		QRCodeSVG:   qrSVG,
	}, nil
}

func buildManifestResponse(rec *storedLink, baseOrigin string, req ManifestRequest) *ManifestResponse {
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
	}
}

// ResolveManifest validates recipient, revocation, expiration, passcode, and maxViews,
// returning the SHLink manifest and associated (tenant, patientID) for AuditEvent logging.
func (m *Manager) ResolveManifest(linkID string, baseOrigin string, req ManifestRequest) (*ManifestResponse, string, string, error) {
	if strings.TrimSpace(req.Recipient) == "" {
		metaTenant, metaPatient := m.LinkOwner(linkID)
		return nil, metaTenant, metaPatient, ErrRecipientRequired
	}
	cleanedID := strings.TrimSpace(linkID)

	if st := m.getBackendStore(); st != nil {
		for range 16 {
			rawJSON, ver, ok, err := st.GetLink(cleanedID)
			if err != nil {
				return nil, "", "", err
			}
			if !ok {
				return nil, "", "", ErrNotFound
			}
			var rec storedLink
			if err := json.Unmarshal(rawJSON, &rec); err != nil {
				return nil, "", "", err
			}
			if rec.Revoked {
				return nil, rec.Tenant, rec.PatientID, ErrRevoked
			}
			now := m.Now()
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
					updatedJSON, mErr := json.Marshal(&rec)
					if mErr != nil {
						return nil, rec.Tenant, rec.PatientID, mErr
					}
					swapped, casErr := st.CompareAndSwapLink(cleanedID, rec.Tenant, ver, updatedJSON)
					if casErr != nil {
						return nil, rec.Tenant, rec.PatientID, casErr
					}
					if !swapped {
						continue
					}
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
			updatedJSON, mErr := json.Marshal(&rec)
			if mErr != nil {
				return nil, rec.Tenant, rec.PatientID, mErr
			}
			swapped, casErr := st.CompareAndSwapLink(cleanedID, rec.Tenant, ver, updatedJSON)
			if casErr != nil {
				return nil, rec.Tenant, rec.PatientID, casErr
			}
			if !swapped {
				continue
			}
			return buildManifestResponse(&rec, baseOrigin, req), rec.Tenant, rec.PatientID, nil
		}
		return nil, "", "", errors.New("shlink: concurrent manifest update conflict")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.links[cleanedID]
	if !ok {
		return nil, "", "", ErrNotFound
	}
	if rec.Revoked {
		return nil, rec.Tenant, rec.PatientID, ErrRevoked
	}
	now := m.nowLocked()
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
	return buildManifestResponse(rec, baseOrigin, req), rec.Tenant, rec.PatientID, nil
}

// ReadContent returns the pre-encrypted compact JWE for a specific SHLink item,
// enforcing revocation and expiration checks.
func (m *Manager) ReadContent(linkID, itemID string) (compactJWE string, tenant string, patientID string, err error) {
	cleanedID := strings.TrimSpace(linkID)
	cleanedItem := strings.TrimSpace(itemID)

	var rec *storedLink
	if st := m.getBackendStore(); st != nil {
		rawJSON, _, ok, gErr := st.GetLink(cleanedID)
		if gErr != nil || !ok {
			return "", "", "", ErrNotFound
		}
		var parsed storedLink
		if json.Unmarshal(rawJSON, &parsed) != nil {
			return "", "", "", ErrNotFound
		}
		rec = &parsed
	} else {
		m.mu.RLock()
		r, ok := m.links[cleanedID]
		if ok && r != nil {
			cp := *r
			cp.Items = append([]EncryptedItem(nil), r.Items...)
			rec = &cp
		}
		m.mu.RUnlock()
	}

	if rec == nil {
		return "", "", "", ErrNotFound
	}
	if rec.Revoked {
		return "", rec.Tenant, rec.PatientID, ErrRevoked
	}
	now := m.Now()
	if !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt) {
		return "", rec.Tenant, rec.PatientID, ErrExpired
	}
	for _, it := range rec.Items {
		if it.ID == cleanedItem {
			return it.CompactJWE, rec.Tenant, rec.PatientID, nil
		}
	}
	return "", rec.Tenant, rec.PatientID, ErrNotFound
}

// Revoke permanently deactivates an issued SMART Health Link for the given tenant and patient.
func (m *Manager) Revoke(tenant, patientID, linkID string) error {
	cleanedID := strings.TrimSpace(linkID)
	if st := m.getBackendStore(); st != nil {
		for range 16 {
			rawJSON, ver, ok, err := st.GetLink(cleanedID)
			if err != nil || !ok {
				return ErrNotFound
			}
			var rec storedLink
			if json.Unmarshal(rawJSON, &rec) != nil {
				return ErrNotFound
			}
			if rec.Tenant != "" && tenant != "" && rec.Tenant != tenant {
				return ErrNotFound
			}
			if patientID != "" && rec.PatientID != "" && rec.PatientID != patientID {
				return ErrForbiddenPatient
			}
			rec.Revoked = true
			rec.Items = nil
			updatedJSON, mErr := json.Marshal(&rec)
			if mErr != nil {
				return mErr
			}
			swapped, casErr := st.CompareAndSwapLink(cleanedID, rec.Tenant, ver, updatedJSON)
			if casErr != nil {
				return casErr
			}
			if swapped {
				return nil
			}
		}
		return errors.New("shlink: concurrent revoke conflict")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.links[cleanedID]
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
	cleanedID := strings.TrimSpace(linkID)
	if st := m.getBackendStore(); st != nil {
		rawJSON, _, ok, err := st.GetLink(cleanedID)
		if err != nil || !ok {
			return "", ""
		}
		var rec storedLink
		if json.Unmarshal(rawJSON, &rec) != nil {
			return "", ""
		}
		return rec.Tenant, rec.PatientID
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.links[cleanedID]
	if !ok {
		return "", ""
	}
	return rec.Tenant, rec.PatientID
}
