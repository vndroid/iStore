package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kane/istore/internal/auximageprovider"
	"github.com/kane/istore/internal/processing"
	"github.com/kane/istore/internal/security"
)

func TestSignedMessage(t *testing.T) {
	tests := []struct {
		path  string
		chain string
		want  string
	}{
		{"/photo.jpg", "", "/photo.jpg"},
		{"/photo.jpg", "image/resize,w_800", "/photo.jpg?x-oss-process=image/resize,w_800"},
		{"/a/b/c.png", "image/info", "/a/b/c.png?x-oss-process=image/info"},
	}

	for _, tt := range tests {
		if got := SignedMessage(tt.path, tt.chain); got != tt.want {
			t.Errorf("SignedMessage(%q, %q) = %q, want %q", tt.path, tt.chain, got, tt.want)
		}
	}
}

// recordingVerifier captures what the server asked it to check.
type recordingVerifier struct {
	enabled bool
	err     error

	gotSignature string
	gotMessage   string
	calls        int
}

func (v *recordingVerifier) SignatureEnabled() bool { return v.enabled }

func (v *recordingVerifier) VerifySignature(_ context.Context, signature, message string) error {
	v.calls++
	v.gotSignature = signature
	v.gotMessage = message
	return v.err
}

// New must drop a verifier that reports signing is unconfigured, so the
// unsigned deployment does not pay for — or answer 403 from — a check it never
// asked for.
func TestNewDropsADisabledVerifier(t *testing.T) {
	newServer := func(v SignatureVerifier) *Server {
		t.Helper()
		s, err := New(
			Config{Root: t.TempDir(), Verifier: v},
			func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil },
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}

	disabled := &recordingVerifier{enabled: false}
	s := newServer(disabled)
	if s.verifier != nil {
		t.Error("a disabled verifier was kept")
	}
	if err := s.verifySignature(httptest.NewRequest(http.MethodGet, "/photo.jpg", nil)); err != nil {
		t.Errorf("verifySignature: %v", err)
	}
	if disabled.calls != 0 {
		t.Errorf("a disabled verifier was called %d times", disabled.calls)
	}

	if s := newServer(&recordingVerifier{enabled: true}); s.verifier == nil {
		t.Error("an enabled verifier was dropped")
	}

	if s := newServer(nil); s.verifier != nil {
		t.Error("a nil verifier produced a non-nil check")
	}
}

func TestVerifySignatureMessage(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantSig string
		wantMsg string
	}{
		{
			name:    "path only",
			target:  "/photo.jpg?x-istore-signature=abc",
			wantSig: "abc",
			wantMsg: "/photo.jpg",
		},
		{
			name:    "path and chain",
			target:  "/photo.jpg?x-oss-process=image/resize,w_800&x-istore-signature=abc",
			wantSig: "abc",
			wantMsg: "/photo.jpg?x-oss-process=image/resize,w_800",
		},
		{
			name:    "signature first",
			target:  "/photo.jpg?x-istore-signature=abc&x-oss-process=image/info",
			wantSig: "abc",
			wantMsg: "/photo.jpg?x-oss-process=image/info",
		},
		{
			// Unrelated parameters are outside the signature on purpose: a
			// cache-buster is added by things that do not have the key.
			name:    "other parameters ignored",
			target:  "/photo.jpg?v=3&x-oss-process=image/info&utm_source=x&x-istore-signature=abc",
			wantSig: "abc",
			wantMsg: "/photo.jpg?x-oss-process=image/info",
		},
		{
			name:    "no signature supplied",
			target:  "/photo.jpg",
			wantSig: "",
			wantMsg: "/photo.jpg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &recordingVerifier{enabled: true}
			s := &Server{verifier: v}

			if err := s.verifySignature(httptest.NewRequest(http.MethodGet, tt.target, nil)); err != nil {
				t.Fatalf("verifySignature: %v", err)
			}
			if v.gotSignature != tt.wantSig {
				t.Errorf("signature = %q, want %q", v.gotSignature, tt.wantSig)
			}
			if v.gotMessage != tt.wantMsg {
				t.Errorf("message = %q, want %q", v.gotMessage, tt.wantMsg)
			}
		})
	}
}

// The message the server builds has to be the one cmd/sign would produce for the
// same URL, or every signed request fails. This pins the two together.
func TestSignedRequestVerifies(t *testing.T) {
	c := security.NewDefaultConfig()
	c.Keys = [][]byte{[]byte("the key")}
	c.Salts = [][]byte{[]byte("the salt")}

	checker, err := security.New(&c)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{verifier: checker}

	target := "/photo.jpg?x-oss-process=image/resize,m_fill,w_200,h_200"
	sig, err := checker.Sign(SignedMessage("/photo.jpg", "image/resize,m_fill,w_200,h_200"))
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, target+"&x-istore-signature="+sig, nil)
	if err := s.verifySignature(r); err != nil {
		t.Errorf("a correctly signed request was rejected: %v", err)
	}

	// Same signature, one character of the chain changed.
	tampered := httptest.NewRequest(http.MethodGet,
		"/photo.jpg?x-oss-process=image/resize,m_fill,w_2000,h_200&x-istore-signature="+sig, nil)
	if err := s.verifySignature(tampered); err == nil {
		t.Error("a tampered chain was accepted")
	}
}

// A rejected signature answers 403 with a body that does not say which way it
// was wrong.
func TestFailSignatureIsOpaque(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/photo.jpg", nil)

	s.failSignature(w, r, errors.New("signature for /other.jpg, key #2"))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if body := w.Body.String(); body != `{"Code":"AccessDenied","Message":"Forbidden"}` {
		t.Errorf("body = %s", body)
	}
}
