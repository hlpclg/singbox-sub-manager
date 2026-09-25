package realitynode

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// RFC 7748 §6.1 X25519 test vector (Alice's keypair), copied verbatim from
// the Go standard library's own crypto/ecdh test vectors
// (src/crypto/ecdh/ecdh_test.go, ecdh.X25519() entry), which is itself
// sourced from the RFC. This is the ground truth for DerivePublicKey: it
// proves the private-key -> public-key derivation this package performs is
// byte-for-byte correct against the standardized vector, not just
// "whatever crypto/ecdh happens to return".
const (
	rfc7748AlicePrivateHex = "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"
	rfc7748AlicePublicHex  = "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"
)

func hexToB64RawURL(t *testing.T, h string) string {
	t.Helper()
	raw, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad test hex %q: %v", h, err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestIdentityVectors(t *testing.T) {
	priv := hexToB64RawURL(t, rfc7748AlicePrivateHex)
	wantPub := hexToB64RawURL(t, rfc7748AlicePublicHex)

	got, err := DerivePublicKey(priv)
	if err != nil {
		t.Fatalf("DerivePublicKey(RFC 7748 Alice private key): %v", err)
	}
	if got != wantPub {
		t.Fatalf("public key mismatch: got %s, want %s", got, wantPub)
	}
}

func TestDerivePublicKeyRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"not base64", "not-valid-base64!!!"},
		{"31 bytes (one short)", hexToB64RawURL(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c")},
		{"33 bytes (one long)", hexToB64RawURL(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2aab")},
		{"empty", ""},
		{"padded base64 (not raw-url)", base64.StdEncoding.EncodeToString(make([]byte, 32))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DerivePublicKey(tt.in)
			if err == nil {
				t.Fatalf("expected error for input %q, got none", tt.in)
			}
			// The private key must never appear in the diagnostic, however
			// malformed the input.
			if tt.in != "" && strings.Contains(err.Error(), tt.in) {
				t.Fatalf("error message leaks input: %v", err)
			}
		})
	}
}

func TestGenerateIdentity(t *testing.T) {
	id, err := GenerateIdentity(newDeterministicReader(1))
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	if !ValidUUIDv4(id.UUID) {
		t.Fatalf("generated UUID %q is not a valid UUIDv4", id.UUID)
	}
	if !ValidGeneratedShortID(id.ShortID) {
		t.Fatalf("generated short_id %q is not 16 hex chars", id.ShortID)
	}
	pub, err := DerivePublicKey(id.PrivateKey)
	if err != nil {
		t.Fatalf("derive public key from generated private key: %v", err)
	}
	if pub == "" {
		t.Fatalf("derived public key is empty")
	}

	// Regenerating must not reuse randomness (basic sanity: two identities
	// from two different deterministic streams must differ).
	id2, err := GenerateIdentity(newDeterministicReader(2))
	if err != nil {
		t.Fatalf("GenerateIdentity (2nd): %v", err)
	}
	if id.UUID == id2.UUID || id.PrivateKey == id2.PrivateKey || id.ShortID == id2.ShortID {
		t.Fatalf("two identities from different random streams unexpectedly matched")
	}
}

// TestGenerateIdentityRandomSourceError injects a reader that fails, and
// asserts the error propagates without a private key ever being formed or
// diagnosed.
func TestGenerateIdentityRandomSourceError(t *testing.T) {
	wantErr := errors.New("injected random source failure")
	_, err := GenerateIdentity(failingReader{err: wantErr})
	if err == nil {
		t.Fatalf("expected error from failing random source, got none")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error does not wrap the injected failure: %v", err)
	}
}

// deterministicReader is a seeded, reproducible byte stream for tests that
// need real (not failing) randomness without depending on the OS RNG.
type deterministicReader struct{ state uint64 }

func newDeterministicReader(seed uint64) *deterministicReader {
	return &deterministicReader{state: seed + 1}
}

func (r *deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state = r.state*6364136223846793005 + 1442695040888963407
		p[i] = byte(r.state >> 56)
	}
	return len(p), nil
}

type failingReader struct{ err error }

func (f failingReader) Read(p []byte) (int, error) { return 0, f.err }

func TestGenerateIdentityShortReadIsError(t *testing.T) {
	// A reader that returns fewer bytes than requested with no error must
	// still surface as a failure (io.ReadFull semantics), not silently
	// truncate an identity field.
	_, err := GenerateIdentity(io.LimitReader(bytes.NewReader(make([]byte, 4)), 4))
	if err == nil {
		t.Fatalf("expected error from short random stream, got none")
	}
}
