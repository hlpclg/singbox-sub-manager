package realitynode

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
)

// Identity is a generated Reality server identity (spec §8.1 step 2):
// UUIDv4, an X25519 keypair (private key only is stored; the public key is
// always derived, per spec §8.3's "公钥实时推导"), and an 8-byte short_id.
type Identity struct {
	UUID       string
	PrivateKey string // base64 raw-url encoded, 32 bytes decoded
	ShortID    string // 16 lowercase hex chars (8 bytes)
}

var (
	uuidV4Re   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	shortIDRe  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	privKeyLen = 32
)

// GenerateIdentity produces a fresh identity using r as the randomness
// source (crypto/rand.Reader in production; injectable so callers can
// exercise the "random source error" path deterministically).
func GenerateIdentity(r io.Reader) (Identity, error) {
	var uuidBytes [16]byte
	if _, err := io.ReadFull(r, uuidBytes[:]); err != nil {
		return Identity{}, fmt.Errorf("generate uuid: %w", err)
	}
	uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x40
	uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80
	uuidStr := fmt.Sprintf("%x-%x-%x-%x-%x",
		uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])

	priv, err := ecdh.X25519().GenerateKey(r)
	if err != nil {
		return Identity{}, fmt.Errorf("generate x25519 key: %w", err)
	}

	var sid [8]byte
	if _, err := io.ReadFull(r, sid[:]); err != nil {
		return Identity{}, fmt.Errorf("generate short_id: %w", err)
	}

	return Identity{
		UUID:       uuidStr,
		PrivateKey: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
		ShortID:    hex.EncodeToString(sid[:]),
	}, nil
}

// DerivePublicKey computes the X25519 public key for a base64-raw-url
// encoded private key (spec §8.3's "公钥实时推导": the public key is never
// stored, only ever recomputed on demand). The error message never echoes
// the input, so a malformed private key cannot leak into diagnostics.
func DerivePublicKey(privateKeyB64 string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(privateKeyB64)
	if err != nil {
		return "", fmt.Errorf("invalid private key encoding")
	}
	if len(raw) != privKeyLen {
		return "", fmt.Errorf("invalid private key length")
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("invalid private key")
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

// ValidUUIDv4 reports whether s is a syntactically valid, lowercase UUIDv4.
func ValidUUIDv4(s string) bool {
	return uuidV4Re.MatchString(s)
}

// ValidGeneratedShortID reports whether s is exactly 16 lowercase hex
// characters (8 bytes), the format GenerateIdentity always produces. This
// is stricter than nodes.ShortID's user-input rule (1-8 byte pairs,
// case-insensitive), which allows short_ids typed by a human.
func ValidGeneratedShortID(s string) bool {
	return shortIDRe.MatchString(s)
}
