package security

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
)

// SignatureEnabled reports whether signature checking is actually configured,
// which needs both ISTORE_KEY and ISTORE_SALT.
//
// Callers use this to decide whether to check at all. VerifySignature is not
// enough for that on its own: with no keys it accepts everything except a
// signature containing a colon, which is imgproxy's guard against a URL whose
// signature segment is really a processing segment — a URL shape iStore does not
// have.
func (s *Checker) SignatureEnabled() bool {
	return len(s.config.Keys) > 0 && len(s.config.Salts) > 0
}

// Sign returns the signature for path under the first configured key and salt.
// It is the inverse of VerifySignature, and what cmd/sign prints.
func (s *Checker) Sign(path string) (string, error) {
	if !s.SignatureEnabled() {
		return "", errors.New("signing needs both ISTORE_KEY and ISTORE_SALT")
	}
	return Sign(path, s.config.Keys[0], s.config.Salts[0], s.config.SignatureSize), nil
}

// Sign returns the HMAC of path under key and salt, truncated to signatureSize
// bytes and base64url-encoded without padding — the encoding VerifySignature
// decodes.
func Sign(path string, key, salt []byte, signatureSize int) string {
	return base64.RawURLEncoding.EncodeToString(signatureFor(path, key, salt, signatureSize))
}

func (s *Checker) VerifySignature(ctx context.Context, signature, path string) error {
	if len(s.config.Keys) == 0 || len(s.config.Salts) == 0 {
		if strings.Contains(signature, ":") {
			return newMalformedSignatureError(ctx)
		}

		return nil
	}

	if slices.Contains(s.config.TrustedSignatures, signature) {
		return nil
	}

	messageMAC, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return newSignatureError("Invalid signature encoding")
	}

	for i := range len(s.config.Keys) {
		if hmac.Equal(messageMAC, signatureFor(path, s.config.Keys[i], s.config.Salts[i], s.config.SignatureSize)) {
			return nil
		}
	}

	return newSignatureError("Invalid signature")
}

func signatureFor(str string, key, salt []byte, signatureSize int) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(salt)

	// It's supposed that path starts with '/'. However, if and input path comes with the
	// leading slash split, let's re-add it here.
	if len(str) == 0 || str[0] != '/' {
		mac.Write([]byte{'/'})
	}

	mac.Write([]byte(str))
	expectedMAC := mac.Sum(nil)
	if signatureSize < 32 {
		return expectedMAC[:signatureSize]
	}
	return expectedMAC
}
