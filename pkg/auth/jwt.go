package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

var (
	ErrInvalidToken       = errors.New("invalid token format")
	ErrUnsupportedAlg     = errors.New("unsupported signing algorithm")
	ErrInvalidSignature   = errors.New("invalid token signature")
	ErrTokenExpired       = errors.New("token has expired")
	ErrTokenNotYetValid   = errors.New("token not yet valid")
	ErrInvalidIssuer      = errors.New("invalid token issuer")
	ErrInvalidAudience    = errors.New("invalid token audience")
	ErrKeyNotFound        = errors.New("signing key not found in JWKS")
	ErrUnsupportedKeyType = errors.New("unsupported key type in JWKS")
)

// JWTHeader holds common JWT JOSE header fields.
type JWTHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid,omitempty"`
	Typ string `json:"typ,omitempty"`
}

// JWTPayload holds standard OIDC / SMART claims.
type JWTPayload struct {
	Issuer    string         `json:"iss,omitempty"`
	Subject   string         `json:"sub,omitempty"`
	Audience  Audience       `json:"aud,omitempty"`
	ExpiresAt int64          `json:"exp,omitempty"`
	NotBefore int64          `json:"nbf,omitempty"`
	IssuedAt  int64          `json:"iat,omitempty"`
	Scope     string         `json:"scope,omitempty"`
	Patient   string         `json:"patient,omitempty"`
	Encounter string         `json:"encounter,omitempty"`
	Extra     map[string]any `json:"-"`
}

// Audience handles 'aud' which can be either a single string or an array of strings in JWTs.
type Audience []string

func (a *Audience) UnmarshalJSON(b []byte) error {
	var single string
	if err := json.Unmarshal(b, &single); err == nil {
		*a = []string{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(b, &multi); err == nil {
		*a = multi
		return nil
	}
	return errors.New("audience must be a string or array of strings")
}

func (a Audience) Contains(target string) bool {
	for _, v := range a {
		if v == target {
			return true
		}
	}
	return false
}

// ParsedToken represents a decomposed JWT.
type ParsedToken struct {
	Header       JWTHeader
	Payload      JWTPayload
	RawHeader    []byte
	RawPayload   []byte
	RawSignature []byte
	SignedData   []byte // header + "." + payload
}

// ParseJWT decomposes and unmarshals an untrusted JWT.
func ParseJWT(raw string) (*ParsedToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}

	headerBytes, err := decodeBase64URL(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: invalid header encoding", ErrInvalidToken)
	}

	payloadBytes, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: invalid payload encoding", ErrInvalidToken)
	}

	sigBytes, err := decodeBase64URL(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: invalid signature encoding", ErrInvalidToken)
	}

	var header JWTHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: invalid header JSON", ErrInvalidToken)
	}

	var payload JWTPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("%w: invalid payload JSON", ErrInvalidToken)
	}

	var extra map[string]any
	if err := json.Unmarshal(payloadBytes, &extra); err == nil {
		payload.Extra = extra
	}

	return &ParsedToken{
		Header:       header,
		Payload:      payload,
		RawHeader:    headerBytes,
		RawPayload:   payloadBytes,
		RawSignature: sigBytes,
		SignedData:   []byte(parts[0] + "." + parts[1]),
	}, nil
}

// VerifyClaims checks exp, nbf, iss, and aud against the given expectations.
func (t *ParsedToken) VerifyClaims(expectedIssuer, expectedAudience string, now time.Time) error {
	p := t.Payload

	if p.ExpiresAt != 0 && now.Unix() > p.ExpiresAt {
		return ErrTokenExpired
	}

	if p.NotBefore != 0 && now.Unix() < p.NotBefore {
		return ErrTokenNotYetValid
	}

	if expectedIssuer != "" && p.Issuer != expectedIssuer {
		return fmt.Errorf("%w: got %q, expected %q", ErrInvalidIssuer, p.Issuer, expectedIssuer)
	}

	if expectedAudience != "" && len(p.Audience) > 0 && !p.Audience.Contains(expectedAudience) {
		return fmt.Errorf("%w: expected audience %q", ErrInvalidAudience, expectedAudience)
	}

	return nil
}

// VerifySignature verifies the token's signature against a public key or secret.
func (t *ParsedToken) VerifySignature(key any) error {
	hasher := sha256.New()
	hasher.Write(t.SignedData)
	hashed := hasher.Sum(nil)

	switch t.Header.Alg {
	case "RS256":
		rsaPub, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: expected *rsa.PublicKey for RS256", ErrUnsupportedKeyType)
		}
		if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashed, t.RawSignature); err != nil {
			return ErrInvalidSignature
		}
		return nil

	case "ES256":
		ecdsaPub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: expected *ecdsa.PublicKey for ES256", ErrUnsupportedKeyType)
		}
		if len(t.RawSignature) != 64 {
			return ErrInvalidSignature
		}
		r := new(big.Int).SetBytes(t.RawSignature[:32])
		s := new(big.Int).SetBytes(t.RawSignature[32:])
		if !ecdsa.Verify(ecdsaPub, hashed, r, s) {
			return ErrInvalidSignature
		}
		return nil

	case "HS256":
		secret, ok := key.([]byte)
		if !ok {
			return fmt.Errorf("%w: expected []byte for HS256", ErrUnsupportedKeyType)
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(t.SignedData)
		expectedSig := mac.Sum(nil)
		if !hmac.Equal(t.RawSignature, expectedSig) {
			return ErrInvalidSignature
		}
		return nil

	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedAlg, t.Header.Alg)
	}
}

// JWK represents a single JSON Web Key from a JWKS set.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// JWKSet represents a set of JSON Web Keys (RFC 7517).
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// PublicKey parses a JWK into a standard Go crypto.PublicKey.
func (k *JWK) PublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nBytes, err := decodeBase64URL(k.N)
		if err != nil {
			return nil, fmt.Errorf("invalid RSA modulus n: %w", err)
		}
		eBytes, err := decodeBase64URL(k.E)
		if err != nil {
			return nil, fmt.Errorf("invalid RSA exponent e: %w", err)
		}

		var eInt int
		if len(eBytes) < 4 {
			buf := make([]byte, 4)
			copy(buf[4-len(eBytes):], eBytes)
			eInt = int(binary.BigEndian.Uint32(buf))
		} else {
			eInt = int(binary.BigEndian.Uint32(eBytes))
		}

		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: eInt,
		}, nil

	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("unsupported EC curve: %s", k.Crv)
		}
		xBytes, err := decodeBase64URL(k.X)
		if err != nil {
			return nil, fmt.Errorf("invalid EC x coordinate: %w", err)
		}
		yBytes, err := decodeBase64URL(k.Y)
		if err != nil {
			return nil, fmt.Errorf("invalid EC y coordinate: %w", err)
		}
		// Uncompressed ANSI X9.62 format: 0x04 || 32-byte X || 32-byte Y
		uncompressed := make([]byte, 65)
		uncompressed[0] = 0x04
		copy(uncompressed[1+32-len(xBytes):33], xBytes)
		copy(uncompressed[33+32-len(yBytes):65], yBytes)

		return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), uncompressed)

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKeyType, k.Kty)
	}
}

// FindKey locates a key matching kid. If kid is empty and only one key exists, returns that key.
func (set *JWKSet) FindKey(kid string) (*JWK, error) {
	if len(set.Keys) == 0 {
		return nil, ErrKeyNotFound
	}
	if kid == "" && len(set.Keys) == 1 {
		return &set.Keys[0], nil
	}
	for i := range set.Keys {
		if set.Keys[i].Kid == kid {
			return &set.Keys[i], nil
		}
	}
	return nil, ErrKeyNotFound
}

func decodeBase64URL(s string) ([]byte, error) {
	// Support unpadded or padded base64url
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
