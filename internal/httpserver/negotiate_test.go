package httpserver

import (
	"testing"

	"github.com/vndroid/istore/internal/imagetype"
)

const chromeAccept = "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"

func TestMIMEQuality(t *testing.T) {
	tests := []struct {
		accept, mime string
		want         float64
	}{
		{chromeAccept, "image/avif", 1},
		{chromeAccept, "image/webp", 1},
		{"image/webp,*/*", "image/avif", 0},
		{"*/*", "image/avif", 0},
		{"IMAGE/AVIF", "image/avif", 1},
		{"image/avif;q=0.9", "image/avif", 0.9},
		{"image/avif;q=0", "image/avif", 0},
		{"image/avif;q=bogus", "image/avif", 0},
		{"image/avif;q=0.2,image/avif;q=0.8", "image/avif", 0.8},
		{"image/avifx", "image/avif", 0},
	}
	for _, tt := range tests {
		if got := mimeQuality(tt.accept, tt.mime); got != tt.want {
			t.Errorf("mimeQuality(%q, %q) = %v, want %v", tt.accept, tt.mime, got, tt.want)
		}
	}
}

func TestNegotiateFormatHonoursQualityAndEncoderSupport(t *testing.T) {
	all := func(imagetype.Type) bool { return true }
	if got := negotiateFormat("image/avif,image/webp", all); got != imagetype.AVIF {
		t.Errorf("equal quality: got %s, want avif", got)
	}
	if got := negotiateFormat("image/avif;q=.2,image/webp;q=1", all); got != imagetype.WEBP {
		t.Errorf("client preference: got %s, want webp", got)
	}
	if got := negotiateFormat("image/avif;q=0,image/webp;q=.5", all); got != imagetype.WEBP {
		t.Errorf("q=0: got %s, want webp", got)
	}

	noAVIF := func(t imagetype.Type) bool { return t != imagetype.AVIF }
	withFallback := negotiateFormat("image/avif,image/webp", noAVIF)
	withoutFallback := negotiateFormat("image/avif", noAVIF)
	if withFallback != imagetype.WEBP || withoutFallback != imagetype.Unknown {
		t.Fatalf("unsupported avif: got %s and %s", withFallback, withoutFallback)
	}
	if acceptKey(withFallback) == acceptKey(withoutFallback) {
		t.Errorf("different outputs share cache key %q", acceptKey(withFallback))
	}
}

func TestAcceptKeyUsesNegotiatedOutput(t *testing.T) {
	if got := acceptKey(imagetype.AVIF); got != "image/avif" {
		t.Errorf("avif key = %q", got)
	}
	if got := acceptKey(imagetype.WEBP); got != "image/webp" {
		t.Errorf("webp key = %q", got)
	}
	if got := acceptKey(imagetype.Unknown); got != "source" {
		t.Errorf("source key = %q", got)
	}
}
