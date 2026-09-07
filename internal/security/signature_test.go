package security

import (
	"context"
	"testing"
)

func testChecker(t *testing.T, keys, salts [][]byte, size int) *Checker {
	t.Helper()

	c := NewDefaultConfig()
	c.Keys = keys
	c.Salts = salts
	c.SignatureSize = size

	checker, err := New(&c)
	if err != nil {
		t.Fatal(err)
	}
	return checker
}

var (
	key  = []byte("the key")
	salt = []byte("the salt")
)

func TestSignatureEnabled(t *testing.T) {
	tests := []struct {
		name  string
		keys  [][]byte
		salts [][]byte
		want  bool
	}{
		{"neither", nil, nil, false},
		{"both", [][]byte{key}, [][]byte{salt}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testChecker(t, tt.keys, tt.salts, 32)
			if got := c.SignatureEnabled(); got != tt.want {
				t.Errorf("SignatureEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A key with no salt is a misconfiguration, not a half-enabled state: Validate
// rejects it before a Checker exists.
func TestKeyWithoutSaltIsRejected(t *testing.T) {
	c := NewDefaultConfig()
	c.Keys = [][]byte{key}

	if _, err := New(&c); err == nil {
		t.Error("New accepted a key with no salt")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	c := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)

	for _, path := range []string{
		"/photo.jpg",
		"/photo.jpg?x-oss-process=image/resize,w_800",
		"/deep/nested/name with spaces.png",
		"/",
	} {
		sig, err := c.Sign(path)
		if err != nil {
			t.Fatalf("Sign(%q): %v", path, err)
		}
		if err := c.VerifySignature(context.Background(), sig, path); err != nil {
			t.Errorf("VerifySignature(%q): %v", path, err)
		}
	}
}

// The whole point of signing the chain along with the path: a signature issued
// for one transform must not carry over to another.
func TestSignatureIsBoundToTheChain(t *testing.T) {
	c := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)

	sig, err := c.Sign("/photo.jpg?x-oss-process=image/resize,w_800")
	if err != nil {
		t.Fatal(err)
	}

	for _, other := range []string{
		"/photo.jpg",
		"/photo.jpg?x-oss-process=image/resize,w_8000",
		"/photo.jpg?x-oss-process=image/resize,w_800/format,avif",
		"/private.jpg?x-oss-process=image/resize,w_800",
	} {
		if err := c.VerifySignature(context.Background(), sig, other); err == nil {
			t.Errorf("signature for the w_800 chain also validated %q", other)
		}
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	issuer := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)
	server := testChecker(t, [][]byte{[]byte("another key")}, [][]byte{salt}, 32)

	sig, err := issuer.Sign("/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.VerifySignature(context.Background(), sig, "/photo.jpg"); err == nil {
		t.Error("a signature from a different key validated")
	}
}

func TestVerifyRejectsMissingAndMalformed(t *testing.T) {
	c := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)

	for _, sig := range []string{"", "not base64!!", "c2hvcnQ"} {
		if err := c.VerifySignature(context.Background(), sig, "/photo.jpg"); err == nil {
			t.Errorf("VerifySignature(%q) accepted", sig)
		}
	}
}

// Key rotation: any configured key/salt pair validates, so the new one can be
// added before the old one is retired.
func TestVerifyAcceptsAnyConfiguredKey(t *testing.T) {
	old := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)
	both := testChecker(t,
		[][]byte{[]byte("new key"), key},
		[][]byte{[]byte("new salt"), salt},
		32,
	)

	sig, err := old.Sign("/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := both.VerifySignature(context.Background(), sig, "/photo.jpg"); err != nil {
		t.Errorf("the retiring key no longer validates: %v", err)
	}
}

// A truncated signature is shorter but still has to match, and a full-length one
// must not be accepted in its place.
func TestSignatureSizeIsEnforced(t *testing.T) {
	short := testChecker(t, [][]byte{key}, [][]byte{salt}, 8)
	full := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)

	shortSig, err := short.Sign("/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := short.VerifySignature(context.Background(), shortSig, "/photo.jpg"); err != nil {
		t.Errorf("an 8-byte signature did not validate against its own config: %v", err)
	}

	fullSig, err := full.Sign("/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := short.VerifySignature(context.Background(), fullSig, "/photo.jpg"); err == nil {
		t.Error("a 32-byte signature validated against an 8-byte config")
	}
}

func TestSignNeedsAKey(t *testing.T) {
	c := testChecker(t, nil, nil, 32)

	if _, err := c.Sign("/photo.jpg"); err == nil {
		t.Error("Sign succeeded with no key configured")
	}
}

// A leading slash is added if the caller left it off, so the two spellings sign
// the same message.
func TestSignNormalisesTheLeadingSlash(t *testing.T) {
	c := testChecker(t, [][]byte{key}, [][]byte{salt}, 32)

	with, err := c.Sign("/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	without, err := c.Sign("photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if with != without {
		t.Errorf("Sign(%q) = %q, Sign(%q) = %q; want equal", "/photo.jpg", with, "photo.jpg", without)
	}
}

func TestTrustedSignatureBypass(t *testing.T) {
	c := NewDefaultConfig()
	c.Keys = [][]byte{key}
	c.Salts = [][]byte{salt}
	c.TrustedSignatures = []string{"letmein"}

	checker, err := New(&c)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.VerifySignature(context.Background(), "letmein", "/anything.jpg"); err != nil {
		t.Errorf("a trusted signature was rejected: %v", err)
	}
}
