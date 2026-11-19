// Package shlink implements the HL7 SMART Health Links (SHLinks) protocol,
// including RFC 7516 compact JWE encryption ("alg":"dir", "enc":"A256GCM"),
// shlink:/ deep-link payload encoding/decoding, pure-Go ISO/IEC 18004 SVG QR
// generation, and zero-knowledge server manifest/passcode/expiry management.
package shlink

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// KeySize is the required 256-bit (32-byte) symmetric key length for A256GCM.
	KeySize = 32
	// GCMNonceSize is the RFC 7518 Section 5.3 96-bit (12-byte) IV length for A256GCM.
	GCMNonceSize = 12
	// GCMTagSize is the 128-bit (16-byte) authentication tag length for A256GCM.
	GCMTagSize = 16
	// MaxInflatedBytes bounds raw DEFLATE decompression to prevent zip bombs (16 MiB).
	MaxInflatedBytes = 16 << 20
	// SHLinkSchemePrefix is the protocol prefix for SMART Health Links URIs.
	SHLinkSchemePrefix = "shlink:/"
)

var (
	ErrInvalidKey        = errors.New("shlink: invalid symmetric key (expected 32 bytes for A256GCM)")
	ErrMalformedJWE      = errors.New("shlink: malformed compact JWE")
	ErrUnsupportedJWEAlg = errors.New("shlink: unsupported JWE alg or enc (expected dir + A256GCM)")
	ErrAuthentication    = errors.New("shlink: JWE authentication tag verification failed")
	ErrMalformedURI      = errors.New("shlink: malformed shlink:/ URI")
)

// JWEProtectedHeader represents the RFC 7516 protected header for SHLink files.
type JWEProtectedHeader struct {
	Alg string `json:"alg"`
	Enc string `json:"enc"`
	Cty string `json:"cty,omitempty"`
	Zip string `json:"zip,omitempty"`
}

// SHLinkPayload represents the JSON object base64url-encoded after "shlink:/".
type SHLinkPayload struct {
	URL   string `json:"url"`
	Key   string `json:"key"`
	Exp   int64  `json:"exp,omitempty"`
	Flag  string `json:"flag,omitempty"`
	Label string `json:"label,omitempty"`
	V     int    `json:"v,omitempty"`
}

// GenerateAES256Key generates a cryptographically random 32-byte key and its
// 43-character unpadded base64url representation per the SHLinks specification.
func GenerateAES256Key() ([]byte, string, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, "", fmt.Errorf("shlink: generate random key: %w", err)
	}
	return key, base64.RawURLEncoding.EncodeToString(key), nil
}

// DecodeAES256Key decodes a 43-character base64url symmetric key into 32 raw bytes.
func DecodeAES256Key(b64Key string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(b64Key))
	if err != nil || len(raw) != KeySize {
		return nil, ErrInvalidKey
	}
	return raw, nil
}

// EncryptCompactJWE encrypts plaintext using RFC 7516 compact serialization with
// "alg":"dir" and "enc":"A256GCM" (and optional raw DEFLATE "zip":"DEF").
func EncryptCompactJWE(key []byte, plaintext []byte, contentType string, deflate bool) (string, error) {
	if len(key) != KeySize {
		return "", ErrInvalidKey
	}

	hdr := JWEProtectedHeader{
		Alg: "dir",
		Enc: "A256GCM",
		Cty: strings.TrimSpace(contentType),
	}
	payload := plaintext
	if deflate {
		hdr.Zip = "DEF"
		var buf bytes.Buffer
		zw, err := flate.NewWriter(&buf, flate.BestCompression)
		if err != nil {
			return "", fmt.Errorf("shlink: init flate writer: %w", err)
		}
		if _, err := zw.Write(plaintext); err != nil {
			_ = zw.Close()
			return "", fmt.Errorf("shlink: deflate write: %w", err)
		}
		if err := zw.Close(); err != nil {
			return "", fmt.Errorf("shlink: deflate close: %w", err)
		}
		payload = buf.Bytes()
	}

	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", fmt.Errorf("shlink: marshal JWE header: %w", err)
	}
	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)

	iv := make([]byte, GCMNonceSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("shlink: generate JWE IV: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("shlink: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("shlink: aes-gcm: %w", err)
	}

	// RFC 7516 Section 5.1 Step 14: AAD is ASCII(BASE64URL(UTF8(JWE Protected Header)))
	sealed := gcm.Seal(nil, iv, payload, []byte(hdrB64))
	if len(sealed) < GCMTagSize {
		return "", ErrMalformedJWE
	}
	ciphertext := sealed[:len(sealed)-GCMTagSize]
	authTag := sealed[len(sealed)-GCMTagSize:]

	// Compact JWE: BASE64URL(Header) || '.' || '' || '.' || BASE64URL(IV) || '.' || BASE64URL(Ciphertext) || '.' || BASE64URL(Tag)
	var b strings.Builder
	b.Grow(len(hdrB64) + 4 + 16 + (len(ciphertext)*4)/3 + 24)
	b.WriteString(hdrB64)
	b.WriteString("..")
	b.WriteString(base64.RawURLEncoding.EncodeToString(iv))
	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(ciphertext))
	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(authTag))
	return b.String(), nil
}

// DecryptCompactJWE verifies and decrypts an RFC 7516 compact JWE ("dir" + "A256GCM").
func DecryptCompactJWE(key []byte, compactJWE string) ([]byte, string, error) {
	if len(key) != KeySize {
		return nil, "", ErrInvalidKey
	}
	parts := strings.Split(strings.TrimSpace(compactJWE), ".")
	if len(parts) != 5 || parts[0] == "" || parts[1] != "" {
		return nil, "", ErrMalformedJWE
	}

	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, "", ErrMalformedJWE
	}
	var hdr JWEProtectedHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, "", ErrMalformedJWE
	}
	if hdr.Alg != "dir" || hdr.Enc != "A256GCM" {
		return nil, "", ErrUnsupportedJWEAlg
	}
	if hdr.Zip != "" && hdr.Zip != "DEF" {
		return nil, "", ErrUnsupportedJWEAlg
	}

	iv, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(iv) != GCMNonceSize {
		return nil, "", ErrMalformedJWE
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, "", ErrMalformedJWE
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil || len(tag) != GCMTagSize {
		return nil, "", ErrMalformedJWE
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}

	combined := make([]byte, 0, len(ciphertext)+len(tag))
	combined = append(combined, ciphertext...)
	combined = append(combined, tag...)

	decrypted, err := gcm.Open(nil, iv, combined, []byte(parts[0]))
	if err != nil {
		return nil, "", ErrAuthentication
	}

	if hdr.Zip == "DEF" {
		fr := flate.NewReader(bytes.NewReader(decrypted))
		defer fr.Close()
		limited := io.LimitReader(fr, MaxInflatedBytes+1)
		inflated, err := io.ReadAll(limited)
		if err != nil {
			return nil, "", fmt.Errorf("shlink: inflate DEF payload: %w", err)
		}
		if len(inflated) > MaxInflatedBytes {
			return nil, "", errors.New("shlink: inflated payload exceeds maximum allowed size")
		}
		decrypted = inflated
	}

	return decrypted, hdr.Cty, nil
}

// EncodeSHLinkURI encodes an SHLinkPayload as a "shlink:/<base64url>" URI
// (or "<viewerURL>#shlink:/<base64url>" when viewerURL is non-empty).
func EncodeSHLinkURI(p SHLinkPayload, viewerURL string) (string, error) {
	p.URL = strings.TrimSpace(p.URL)
	p.Key = strings.TrimSpace(p.Key)
	if p.URL == "" {
		return "", errors.New("shlink: manifest url is required")
	}
	if _, err := DecodeAES256Key(p.Key); err != nil {
		return "", err
	}
	if len([]rune(p.Label)) > 80 {
		runes := []rune(p.Label)
		p.Label = string(runes[:80])
	}
	if p.V == 0 {
		p.V = 1
	}
	rawJSON, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	core := SHLinkSchemePrefix + base64.RawURLEncoding.EncodeToString(rawJSON)
	viewer := strings.TrimSpace(viewerURL)
	if viewer == "" {
		return core, nil
	}
	viewer = strings.TrimRight(viewer, "#")
	return viewer + "#" + core, nil
}

// DecodeSHLinkURI parses either a bare "shlink:/<base64url>" URI or a viewer
// link containing "#shlink:/<base64url>" and validates its required fields.
func DecodeSHLinkURI(uri string) (SHLinkPayload, error) {
	trimmed := strings.TrimSpace(uri)
	if idx := strings.Index(trimmed, "#"+SHLinkSchemePrefix); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	if !strings.HasPrefix(trimmed, SHLinkSchemePrefix) {
		return SHLinkPayload{}, ErrMalformedURI
	}
	encoded := strings.TrimPrefix(trimmed, SHLinkSchemePrefix)
	if encoded == "" {
		return SHLinkPayload{}, ErrMalformedURI
	}
	rawJSON, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return SHLinkPayload{}, ErrMalformedURI
	}
	var p SHLinkPayload
	if err := json.Unmarshal(rawJSON, &p); err != nil {
		return SHLinkPayload{}, ErrMalformedURI
	}
	if strings.TrimSpace(p.URL) == "" {
		return SHLinkPayload{}, ErrMalformedURI
	}
	if _, err := DecodeAES256Key(p.Key); err != nil {
		return SHLinkPayload{}, err
	}
	return p, nil
}
