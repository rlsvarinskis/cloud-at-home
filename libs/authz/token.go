package authz

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/ids"
)

type claims struct {
	jwt.RegisteredClaims
	Type     PrincipalType `json:"typ"`
	Bindings []Binding     `json:"b"`
}

// Issuer mints tokens with one Ed25519 private key. Used by hcctl (user tokens) and by services
// that call other services (service tokens). Servers only ever hold the matching *public* keys.
type Issuer struct {
	KeyID string
	Key   ed25519.PrivateKey
	Now   func() time.Time // injectable for tests
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Issuer) Mint(p Principal, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", errors.New("ttl must be positive")
	}
	for _, b := range p.Bindings {
		if !KnownRole(b.Role) {
			return "", fmt.Errorf("unknown role %q", b.Role)
		}
	}
	now := i.now()
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.KeyID,
			Subject:   p.Subject,
			ID:        ids.New("tok"),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Type:     p.Type,
		Bindings: p.Bindings,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	t.Header["kid"] = i.KeyID
	return t.SignedString(i.Key)
}

// TrustedKey is a public key a server accepts tokens from, optionally limited to certain roles so
// that e.g. agentd's key can never be used to mint an admin token.
type TrustedKey struct {
	Public       ed25519.PublicKey
	AllowedRoles []Role // empty = any role
}

// Revocations lets a deployment deny individual token IDs before they expire.
type Revocations interface{ Revoked(jti string) bool }

type Verifier struct {
	Keys        map[string]TrustedKey
	MaxTTL      time.Duration // reject tokens valid for longer than this (0 = no limit)
	Revocations Revocations
	Now         func() time.Time
}

func (v *Verifier) Verify(token string) (*Principal, error) {
	var c claims
	parsed, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, ok := v.Keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown key id %q", kid)
		}
		return k.Public, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithExpirationRequired(), jwt.WithStrictDecoding(), jwt.WithTimeFunc(v.timeFunc()),
		jwt.WithLeeway(30*time.Second))
	if err != nil || !parsed.Valid {
		return nil, fmt.Errorf("%w: %v", errs.ErrUnauthenticated, err)
	}
	kid := parsed.Header["kid"].(string)
	if c.Issuer != kid {
		return nil, fmt.Errorf("%w: issuer does not match key id", errs.ErrUnauthenticated)
	}
	if v.MaxTTL > 0 && c.ExpiresAt.Sub(c.IssuedAt.Time) > v.MaxTTL+time.Minute {
		return nil, fmt.Errorf("%w: token lifetime exceeds the allowed maximum", errs.ErrUnauthenticated)
	}
	if v.Revocations != nil && v.Revocations.Revoked(c.ID) {
		return nil, fmt.Errorf("%w: token revoked", errs.ErrUnauthenticated)
	}
	if allowed := v.Keys[kid].AllowedRoles; len(allowed) > 0 {
		for _, b := range c.Bindings {
			if !contains(allowed, b.Role) {
				return nil, fmt.Errorf("%w: key %q may not grant role %q", errs.ErrUnauthenticated, kid, b.Role)
			}
		}
	}
	for _, b := range c.Bindings {
		if !KnownRole(b.Role) || b.Project == "" {
			return nil, fmt.Errorf("%w: malformed binding", errs.ErrUnauthenticated)
		}
	}
	return &Principal{Subject: c.Subject, Type: c.Type, Bindings: c.Bindings, TokenID: c.ID}, nil
}

func (v *Verifier) timeFunc() func() time.Time {
	if v.Now != nil {
		return v.Now
	}
	return time.Now
}

func contains(rs []Role, r Role) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// ---- key files (PKCS#8 private, PKIX public, PEM) ------------------------------------------------

func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func SavePrivateKey(path string, k ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

func SavePublicKey(path string, k ed25519.PublicKey) error {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
}

func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("no PEM data in " + path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return ek, nil
}

func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("no PEM data in " + path)
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 public key")
	}
	return ek, nil
}
