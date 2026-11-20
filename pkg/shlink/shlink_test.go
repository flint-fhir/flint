package shlink_test

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/pkg/shlink"
)

func TestJWE_RoundTripAndTamperRejection(t *testing.T) {
	key, keyB64, err := shlink.GenerateAES256Key()
	require.NoError(t, err)
	assert.Len(t, key, shlink.KeySize)
	assert.Len(t, keyB64, 43)

	decodedKey, err := shlink.DecodeAES256Key(keyB64)
	require.NoError(t, err)
	assert.Equal(t, key, decodedKey)

	_, err = shlink.DecodeAES256Key("short")
	require.ErrorIs(t, err, shlink.ErrInvalidKey)
	_, err = shlink.EncryptCompactJWE([]byte("short"), []byte("x"), "application/fhir+json", false)
	require.ErrorIs(t, err, shlink.ErrInvalidKey)
	_, _, err = shlink.DecryptCompactJWE([]byte("short"), "a..b.c.d")
	require.ErrorIs(t, err, shlink.ErrInvalidKey)

	plaintext := []byte(`{"resourceType":"Bundle","type":"document","entry":[{"resource":{"resourceType":"Patient","id":"pat-1"}}]}`)
	for _, deflate := range []bool{false, true} {
		jwe, encErr := shlink.EncryptCompactJWE(key, plaintext, "application/fhir+json", deflate)
		require.NoError(t, encErr)

		parts := strings.Split(jwe, ".")
		require.Len(t, parts, 5)
		assert.Empty(t, parts[1], "alg=dir must have empty JWE Encrypted Key segment")

		gotPlain, gotCty, decErr := shlink.DecryptCompactJWE(key, jwe)
		require.NoError(t, decErr)
		assert.Equal(t, plaintext, gotPlain)
		assert.Equal(t, "application/fhir+json", gotCty)

		// Wrong key MUST fail authentication
		wrongKey, _, _ := shlink.GenerateAES256Key()
		_, _, err = shlink.DecryptCompactJWE(wrongKey, jwe)
		require.ErrorIs(t, err, shlink.ErrAuthentication)

		// Tampered authTag MUST fail authentication
		tagBytes, _ := base64.RawURLEncoding.DecodeString(parts[4])
		tagBytes[0] ^= 0xFF
		badTagJWE := strings.Join([]string{parts[0], "", parts[2], parts[3], base64.RawURLEncoding.EncodeToString(tagBytes)}, ".")
		_, _, err = shlink.DecryptCompactJWE(key, badTagJWE)
		require.ErrorIs(t, err, shlink.ErrAuthentication)

		// Tampered ciphertext MUST fail authentication
		ctBytes, _ := base64.RawURLEncoding.DecodeString(parts[3])
		ctBytes[0] ^= 0x01
		badCTJWE := strings.Join([]string{parts[0], "", parts[2], base64.RawURLEncoding.EncodeToString(ctBytes), parts[4]}, ".")
		_, _, err = shlink.DecryptCompactJWE(key, badCTJWE)
		require.ErrorIs(t, err, shlink.ErrAuthentication)

		// Tampered protected header (AAD) MUST fail authentication
		altHdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"dir","enc":"A256GCM","cty":"text/plain"}`))
		badAADJWE := strings.Join([]string{altHdr, "", parts[2], parts[3], parts[4]}, ".")
		_, _, err = shlink.DecryptCompactJWE(key, badAADJWE)
		require.ErrorIs(t, err, shlink.ErrAuthentication)
	}

	// Malformed & unsupported algorithm cases
	for _, bad := range []string{
		"not-five-parts",
		"a.nonemptykey.c.d.e",
		"!!!..b.c.d",
		base64.RawURLEncoding.EncodeToString([]byte(`not-json`)) + "..b.c.d",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"dir","enc":"A256GCM"}`)) + "..shortiv.c.d",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"dir","enc":"A256GCM"}`)) + ".." + base64.RawURLEncoding.EncodeToString(make([]byte, 12)) + ".!!!.d",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"dir","enc":"A256GCM"}`)) + ".." + base64.RawURLEncoding.EncodeToString(make([]byte, 12)) + ".YWJj.shorttag",
	} {
		_, _, err = shlink.DecryptCompactJWE(key, bad)
		require.ErrorIs(t, err, shlink.ErrMalformedJWE, "input=%s", bad)
	}

	unsupAlg := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RSA-OAEP","enc":"A256GCM"}`)) + "..b.c.d"
	_, _, err = shlink.DecryptCompactJWE(key, unsupAlg)
	require.ErrorIs(t, err, shlink.ErrUnsupportedJWEAlg)

	unsupZip := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"dir","enc":"A256GCM","zip":"GZIP"}`)) + "..b.c.d"
	_, _, err = shlink.DecryptCompactJWE(key, unsupZip)
	require.ErrorIs(t, err, shlink.ErrUnsupportedJWEAlg)
}

func TestSHLinkURI_And_QRSVG(t *testing.T) {
	_, keyB64, err := shlink.GenerateAES256Key()
	require.NoError(t, err)

	longLabel := strings.Repeat("Clinical Summary ", 10) // > 80 chars
	p := shlink.SHLinkPayload{
		URL:   "https://fhir.example.org/shlink/abc123/manifest",
		Key:   keyB64,
		Exp:   1893456000,
		Flag:  "LP",
		Label: longLabel,
	}

	bareURI, err := shlink.EncodeSHLinkURI(p, "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(bareURI, "shlink:/"))

	decodedBare, err := shlink.DecodeSHLinkURI(bareURI)
	require.NoError(t, err)
	assert.Equal(t, p.URL, decodedBare.URL)
	assert.Equal(t, p.Key, decodedBare.Key)
	assert.Equal(t, p.Exp, decodedBare.Exp)
	assert.Equal(t, "LP", decodedBare.Flag)
	assert.Equal(t, 1, decodedBare.V)
	assert.Len(t, []rune(decodedBare.Label), 80)

	viewerURI, err := shlink.EncodeSHLinkURI(p, "https://viewer.example.org/open#")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(viewerURI, "https://viewer.example.org/open#shlink:/"))
	decodedViewer, err := shlink.DecodeSHLinkURI(viewerURI)
	require.NoError(t, err)
	assert.Equal(t, decodedBare, decodedViewer)

	// Invalid URI cases
	_, err = shlink.EncodeSHLinkURI(shlink.SHLinkPayload{Key: keyB64}, "")
	require.Error(t, err)
	_, err = shlink.EncodeSHLinkURI(shlink.SHLinkPayload{URL: "https://x/m", Key: "bad"}, "")
	require.ErrorIs(t, err, shlink.ErrInvalidKey)
	for _, badURI := range []string{"", "https://example.org", "shlink:/", "shlink:/!!!", "shlink:/" + base64.RawURLEncoding.EncodeToString([]byte(`{}`))} {
		_, err = shlink.DecodeSHLinkURI(badURI)
		require.Error(t, err)
	}

	// Render QR SVG across short, medium (v7+), and large (v10+ two-group RS) payloads
	for _, sample := range []string{"shlink:/short", bareURI, strings.Repeat("A", 420)} {
		svgStr, qErr := shlink.RenderQRSVG(sample, 0)
		require.NoError(t, qErr)
		assert.Contains(t, svgStr, "<svg")
		var parsed any
		require.NoError(t, xml.Unmarshal([]byte(svgStr), &parsed), "SVG must be valid XML")
	}

	_, err = shlink.RenderQRSVG("   ", 4)
	require.Error(t, err)
	_, err = shlink.RenderQRSVG(strings.Repeat("X", 2000), 4)
	require.Error(t, err)
}

func TestManager_LifecyclePasscodeExpiryMaxViewsAndConcurrency(t *testing.T) {
	mgr := shlink.NewManager()
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	mgr.SetClock(func() time.Time { return now })

	_, err := mgr.CreateLink(shlink.CreateRequest{Tenant: "demo", PatientID: "pat-1"})
	require.Error(t, err, "empty artifacts must fail")

	issued, err := mgr.CreateLink(shlink.CreateRequest{
		Tenant:              "demo",
		PatientID:           "pat-alice-001",
		BaseOrigin:          "https://flint.example.org",
		Label:               "Alice IPS + Immunization Card",
		Passcode:            "482910",
		ExpiresAt:           now.Add(15 * time.Minute),
		MaxViews:            2,
		MaxPasscodeFailures: 3,
		LongTerm:            true,
		Artifacts: []shlink.ArtifactInput{
			{ContentType: "application/smart-health-card", Plaintext: []byte(`{"verifiableCredential":["eyJ..."]}`), Deflate: false},
			{ContentType: "application/fhir+json", Plaintext: []byte(`{"resourceType":"Bundle","type":"document"}`), Deflate: true},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "LP", issued.Flag)
	assert.NotEmpty(t, issued.QRCodeSVG)

	decodedPayload, err := shlink.DecodeSHLinkURI(issued.SHLinkURI)
	require.NoError(t, err)
	symKey, err := shlink.DecodeAES256Key(decodedPayload.Key)
	require.NoError(t, err)

	// Missing recipient rejected
	_, _, _, err = mgr.ResolveManifest(issued.ID, "https://flint.example.org", shlink.ManifestRequest{Passcode: "482910"})
	require.ErrorIs(t, err, shlink.ErrRecipientRequired)

	// Unknown link rejected
	_, _, _, err = mgr.ResolveManifest("nonexistent", "https://flint.example.org", shlink.ManifestRequest{Recipient: "Dr. Bob"})
	require.ErrorIs(t, err, shlink.ErrNotFound)

	// Wrong passcode decrements remaining attempts
	_, _, _, err = mgr.ResolveManifest(issued.ID, "https://flint.example.org", shlink.ManifestRequest{Recipient: "Dr. Bob", Passcode: "000000"})
	require.ErrorIs(t, err, shlink.ErrPasscodeRequired)
	var pe *shlink.PasscodeError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 2, pe.RemainingAttempts)

	// Correct passcode succeeds (view 1 of 2), with inline embedded JWE when embeddedLengthMax is large
	embedMax := 4096
	manifest, ten, pat, err := mgr.ResolveManifest(issued.ID, "https://flint.example.org", shlink.ManifestRequest{
		Recipient:         "Dr. Bob",
		Passcode:          "482910",
		EmbeddedLengthMax: &embedMax,
	})
	require.NoError(t, err)
	assert.Equal(t, "demo", ten)
	assert.Equal(t, "pat-alice-001", pat)
	assert.Equal(t, "can-change", manifest.Status)
	require.Len(t, manifest.Files, 2)
	assert.NotEmpty(t, manifest.Files[0].Embedded)

	// ReadContent and decrypt both items with client-side key
	jwe0, _, _, err := mgr.ReadContent(issued.ID, "item-0")
	require.NoError(t, err)
	plain0, cty0, err := shlink.DecryptCompactJWE(symKey, jwe0)
	require.NoError(t, err)
	assert.Equal(t, "application/smart-health-card", cty0)
	assert.Contains(t, string(plain0), "verifiableCredential")

	jwe1, _, _, err := mgr.ReadContent(issued.ID, "item-1")
	require.NoError(t, err)
	plain1, cty1, err := shlink.DecryptCompactJWE(symKey, jwe1)
	require.NoError(t, err)
	assert.Equal(t, "application/fhir+json", cty1)
	assert.Contains(t, string(plain1), `"Bundle"`)

	_, _, _, err = mgr.ReadContent(issued.ID, "item-99")
	require.ErrorIs(t, err, shlink.ErrNotFound)

	// Second manifest access succeeds (view 2 of 2)
	_, _, _, err = mgr.ResolveManifest(issued.ID, "https://flint.example.org", shlink.ManifestRequest{
		Recipient: "Dr. Bob",
		Passcode:  "482910",
	})
	require.NoError(t, err)

	// Third manifest access hits maxViews (410)
	_, _, _, err = mgr.ResolveManifest(issued.ID, "https://flint.example.org", shlink.ManifestRequest{
		Recipient: "Dr. Bob",
		Passcode:  "482910",
	})
	require.ErrorIs(t, err, shlink.ErrMaxViewsReached)

	// Wrong patient cannot revoke
	err = mgr.Revoke("demo", "pat-other", issued.ID)
	require.ErrorIs(t, err, shlink.ErrForbiddenPatient)

	// Advance clock past expiration -> ReadContent and ResolveManifest return ErrExpired
	now = now.Add(20 * time.Minute)
	_, _, _, err = mgr.ReadContent(issued.ID, "item-0")
	require.ErrorIs(t, err, shlink.ErrExpired)

	// Revoke by rightful patient -> ErrRevoked
	require.NoError(t, mgr.Revoke("demo", "pat-alice-001", issued.ID))
	_, _, _, err = mgr.ReadContent(issued.ID, "item-0")
	require.ErrorIs(t, err, shlink.ErrRevoked)

	// Concurrent maxViews conservation under -race
	now = time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	concLink, err := mgr.CreateLink(shlink.CreateRequest{
		Tenant:     "demo",
		PatientID:  "pat-conc",
		BaseOrigin: "https://flint.example.org",
		MaxViews:   5,
		Artifacts:  []shlink.ArtifactInput{{ContentType: "application/fhir+json", Plaintext: []byte(`{}`)}},
	})
	require.NoError(t, err)

	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 25 {
		wg.Go(func() {
			_, _, _, mErr := mgr.ResolveManifest(concLink.ID, "https://flint.example.org", shlink.ManifestRequest{Recipient: "Clinic"})
			if mErr == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(5), successes.Load(), "strictly MaxViews=5 concurrent callers may succeed")
}

type shlinkLifecycleMachine struct {
	mgr         *shlink.Manager
	now         time.Time
	expiresAt   time.Time
	linkID      string
	symKey      []byte
	passcode    string
	maxViews    int
	maxFailures int
	viewsUsed   int
	failures    int
	revoked     bool
}

func (m *shlinkLifecycleMachine) RuleValidManifest(tc hegel.TestCase) {
	_, _, _, err := m.mgr.ResolveManifest(m.linkID, "https://pbt.example.org", shlink.ManifestRequest{
		Recipient: "PBT Verifier",
		Passcode:  m.passcode,
	})
	expectLocked := m.failures >= m.maxFailures
	expectExpired := !m.now.Before(m.expiresAt)
	if m.revoked {
		if !errors.Is(err, shlink.ErrRevoked) {
			tc.Errorf("expected ErrRevoked, got %v", err)
		}
		return
	}
	if expectExpired {
		if !errors.Is(err, shlink.ErrExpired) {
			tc.Errorf("expected ErrExpired, got %v", err)
		}
		return
	}
	if expectLocked {
		if !errors.Is(err, shlink.ErrPasscodeLockedOut) {
			tc.Errorf("expected ErrPasscodeLockedOut, got %v", err)
		}
		return
	}
	if m.viewsUsed >= m.maxViews {
		if !errors.Is(err, shlink.ErrMaxViewsReached) {
			tc.Errorf("expected ErrMaxViewsReached, got %v", err)
		}
		return
	}
	if err != nil {
		tc.Errorf("expected manifest resolution success, got %v", err)
	}
	m.viewsUsed++
}

func (m *shlinkLifecycleMachine) RuleWrongPasscode(tc hegel.TestCase) {
	wrong := hegel.Draw(tc, hegel.FromRegex(`wrong-[0-9]{3}`, true))
	_, _, _, err := m.mgr.ResolveManifest(m.linkID, "https://pbt.example.org", shlink.ManifestRequest{
		Recipient: "Attacker",
		Passcode:  wrong,
	})
	if err == nil {
		tc.Errorf("wrong passcode must never succeed")
	}
	if !m.revoked && m.now.Before(m.expiresAt) && m.failures < m.maxFailures {
		m.failures++
	}
}

func (m *shlinkLifecycleMachine) RuleAdvanceClock(tc hegel.TestCase) {
	mins := hegel.Draw(tc, hegel.Integers(1, 20))
	m.now = m.now.Add(time.Duration(mins) * time.Minute)
}

func (m *shlinkLifecycleMachine) RuleRevoke(tc hegel.TestCase) {
	if hegel.Draw(tc, hegel.WeightedBooleans(0.15)) {
		if err := m.mgr.Revoke("demo", "pat-pbt", m.linkID); err != nil {
			tc.Errorf("revoke failed: %v", err)
		}
		m.revoked = true
	}
}

func (m *shlinkLifecycleMachine) InvariantBounds(tc hegel.TestCase) {
	if m.viewsUsed > m.maxViews {
		tc.Errorf("viewsUsed (%d) exceeded maxViews (%d)", m.viewsUsed, m.maxViews)
	}
	jwe, _, _, err := m.mgr.ReadContent(m.linkID, "item-0")
	if m.revoked || !m.now.Before(m.expiresAt) {
		if err == nil {
			tc.Errorf("ReadContent must fail when revoked=%v or expired", m.revoked)
		}
		return
	}
	if err != nil {
		tc.Errorf("unexpected ReadContent error: %v", err)
	}
	plain, _, decErr := shlink.DecryptCompactJWE(m.symKey, jwe)
	if decErr != nil || !bytes.Equal(plain, []byte(`{"resourceType":"Patient","id":"pat-pbt"}`)) {
		tc.Errorf("JWE decryption invariant failed: %v", decErr)
	}
}

func TestProperty_SHLinkJWEAndLifecycleInvariants(t *testing.T) {
	t.Run("JWE_CryptographicRoundTripAndTamperRejection", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			body := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z0-9_{}":,\[\]\- ]{0,512}`, true))
			cty := hegel.Draw(ht, hegel.SampledFrom([]string{"application/fhir+json", "application/smart-health-card"}))
			deflate := hegel.Draw(ht, hegel.WeightedBooleans(0.5))

			key, keyB64, err := shlink.GenerateAES256Key()
			if err != nil {
				ht.Fatalf("GenerateAES256Key: %v", err)
			}
			jwe, err := shlink.EncryptCompactJWE(key, []byte(body), cty, deflate)
			if err != nil {
				ht.Fatalf("EncryptCompactJWE: %v", err)
			}
			got, gotCty, err := shlink.DecryptCompactJWE(key, jwe)
			if err != nil {
				ht.Fatalf("DecryptCompactJWE: %v", err)
			}
			if string(got) != body || gotCty != cty {
				ht.Fatalf("round-trip mismatch: got (%q, %q), want (%q, %q)", string(got), gotCty, body, cty)
			}

			// Mutate a random byte in IV or Tag and assert 100% authentication rejection
			parts := strings.Split(jwe, ".")
			segIdx := hegel.Draw(ht, hegel.SampledFrom([]int{2, 4}))
			rawSeg, _ := base64.RawURLEncoding.DecodeString(parts[segIdx])
			bytePos := hegel.Draw(ht, hegel.Integers(0, len(rawSeg)-1))
			rawSeg[bytePos] ^= 0x80
			parts[segIdx] = base64.RawURLEncoding.EncodeToString(rawSeg)
			if _, _, tErr := shlink.DecryptCompactJWE(key, strings.Join(parts, ".")); tErr == nil {
				ht.Fatalf("tampered segment %d was accepted", segIdx)
			}

			// URI + QR SVG round-trip
			uri, err := shlink.EncodeSHLinkURI(shlink.SHLinkPayload{
				URL:   "https://flint.fhir.org/shlink/id1/manifest",
				Key:   keyB64,
				Label: "PBT Share",
			}, "")
			if err != nil {
				ht.Fatalf("EncodeSHLinkURI: %v", err)
			}
			decURI, err := shlink.DecodeSHLinkURI(uri)
			if err != nil || decURI.Key != keyB64 {
				ht.Fatalf("DecodeSHLinkURI failed: %v", err)
			}
		}, hegel.WithTestCases(100))
	})

	t.Run("StatefulPasscodeExpiryMaxViewsMachine", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			mgr := shlink.NewManager()
			start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
			maxViews := hegel.Draw(ht, hegel.Integers(1, 5))
			maxFail := hegel.Draw(ht, hegel.Integers(2, 4))
			passcode := hegel.Draw(ht, hegel.FromRegex(`[0-9]{6}`, true))
			expMins := hegel.Draw(ht, hegel.Integers(15, 60))

			machine := &shlinkLifecycleMachine{
				mgr:         mgr,
				now:         start,
				expiresAt:   start.Add(time.Duration(expMins) * time.Minute),
				passcode:    passcode,
				maxViews:    maxViews,
				maxFailures: maxFail,
			}
			mgr.SetClock(func() time.Time { return machine.now })

			issued, err := mgr.CreateLink(shlink.CreateRequest{
				Tenant:              "demo",
				PatientID:           "pat-pbt",
				BaseOrigin:          "https://pbt.example.org",
				Passcode:            passcode,
				ExpiresAt:           machine.expiresAt,
				MaxViews:            maxViews,
				MaxPasscodeFailures: maxFail,
				Artifacts: []shlink.ArtifactInput{
					{ContentType: "application/fhir+json", Plaintext: []byte(`{"resourceType":"Patient","id":"pat-pbt"}`), Deflate: true},
				},
			})
			if err != nil {
				ht.Fatalf("CreateLink: %v", err)
			}
			p, err := shlink.DecodeSHLinkURI(issued.SHLinkURI)
			if err != nil {
				ht.Fatalf("DecodeSHLinkURI: %v", err)
			}
			key, err := shlink.DecodeAES256Key(p.Key)
			if err != nil {
				ht.Fatalf("DecodeAES256Key: %v", err)
			}
			machine.linkID = issued.ID
			machine.symKey = key

			hegel.RunStateful(
				ht,
				machine,
				hegel.WithStatefulStepCount(25),
				hegel.WithAlwaysCheckInvariants("InvariantBounds"),
			)
		}, hegel.WithTestCases(80))
	})
}

type memBackendEntry struct {
	raw []byte
	ver int64
}

type memBackendStore struct {
	mu      sync.Mutex
	entries map[string]memBackendEntry
}

func newMemBackendStore() *memBackendStore {
	return &memBackendStore{entries: make(map[string]memBackendEntry)}
}

func (s *memBackendStore) CreateLink(id, _ string, rawJSON []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[id] = memBackendEntry{raw: append([]byte(nil), rawJSON...), ver: 1}
	return nil
}

func (s *memBackendStore) GetLink(id string) ([]byte, int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil, 0, false, nil
	}
	return append([]byte(nil), e.raw...), e.ver, true, nil
}

func (s *memBackendStore) CompareAndSwapLink(id, _ string, expectedVersion int64, rawJSON []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || e.ver != expectedVersion {
		return false, nil
	}
	s.entries[id] = memBackendEntry{raw: append([]byte(nil), rawJSON...), ver: e.ver + 1}
	return true, nil
}

func TestManager_BackendStoreCrossReplicaAndCAS(t *testing.T) {
	shared := newMemBackendStore()
	nodeA := shlink.NewManager()
	nodeA.SetBackendStore(shared)
	nodeB := shlink.NewManager()
	nodeB.SetBackendStore(shared)

	issued, err := nodeA.CreateLink(shlink.CreateRequest{
		Tenant:              "demo",
		PatientID:           "pat-cluster-1",
		BaseOrigin:          "https://cluster.flint.org",
		Passcode:            "918273",
		MaxViews:            2,
		MaxPasscodeFailures: 2,
		Artifacts: []shlink.ArtifactInput{
			{ContentType: "application/fhir+json", Plaintext: []byte(`{"resourceType":"Patient","id":"pat-cluster-1"}`), Deflate: true},
		},
	})
	require.NoError(t, err)

	ten, pat := nodeB.LinkOwner(issued.ID)
	assert.Equal(t, "demo", ten)
	assert.Equal(t, "pat-cluster-1", pat)

	// Wrong passcode on nodeB increments shared failure counter
	_, _, _, err = nodeB.ResolveManifest(issued.ID, "https://cluster.flint.org", shlink.ManifestRequest{
		Recipient: "Dr. CrossNode",
		Passcode:  "000000",
	})
	require.ErrorIs(t, err, shlink.ErrPasscodeRequired)

	// Concurrent manifest fetches across nodeA and nodeB with MaxViews=2: exactly 2 succeed!
	var wg sync.WaitGroup
	var okCount int
	var countMu sync.Mutex
	for i := range 6 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := nodeA
			if idx%2 == 1 {
				target = nodeB
			}
			_, _, _, mErr := target.ResolveManifest(issued.ID, "https://cluster.flint.org", shlink.ManifestRequest{
				Recipient: "Dr. CrossNode",
				Passcode:  "918273",
			})
			if mErr == nil {
				countMu.Lock()
				okCount++
				countMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 2, okCount, "optimistic CAS must enforce exact MaxViews=2 across replicas")

	jwe, _, _, err := nodeB.ReadContent(issued.ID, "item-0")
	require.NoError(t, err)
	assert.NotEmpty(t, jwe)

	// Wrong patient forbidden on Revoke, then valid Revoke on nodeA blocks ReadContent & ResolveManifest on nodeB
	require.ErrorIs(t, nodeA.Revoke("demo", "wrong-pat", issued.ID), shlink.ErrForbiddenPatient)
	require.NoError(t, nodeA.Revoke("demo", "pat-cluster-1", issued.ID))
	_, _, _, err = nodeB.ReadContent(issued.ID, "item-0")
	require.ErrorIs(t, err, shlink.ErrRevoked)
	_, _, _, err = nodeB.ResolveManifest(issued.ID, "https://cluster.flint.org", shlink.ManifestRequest{
		Recipient: "Dr. CrossNode",
		Passcode:  "918273",
	})
	require.ErrorIs(t, err, shlink.ErrRevoked)
}
