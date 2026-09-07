package httpserver

import "testing"

const chromeAccept = "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"

func TestAcceptsMIME(t *testing.T) {
	tests := []struct {
		accept, mime string
		want         bool
	}{
		{chromeAccept, "image/avif", true},
		{chromeAccept, "image/webp", true},
		{chromeAccept, "image/jxl", false},
		{"image/webp,*/*", "image/avif", false}, // a wildcard is not a promise
		{"*/*", "image/avif", false},
		{"", "image/avif", false},
		{"IMAGE/AVIF", "image/avif", true},                // case-insensitive
		{"image/avif;q=0.9", "image/avif", true},          // parameters ignored
		{" image/avif , image/webp ", "image/webp", true}, // whitespace tolerated
		{"image/avifx", "image/avif", false},              // no prefix matching
	}
	for _, tt := range tests {
		if got := acceptsMIME(tt.accept, tt.mime); got != tt.want {
			t.Errorf("acceptsMIME(%q, %q) = %v, want %v", tt.accept, tt.mime, got, tt.want)
		}
	}
}

func TestAcceptKey(t *testing.T) {
	// Different orderings with the same capability must share a cache entry.
	a := acceptKey("image/avif,image/webp")
	b := acceptKey("image/webp,image/avif")
	if a != b {
		t.Errorf("orderings gave different keys: %q vs %q", a, b)
	}
	if a != "image/avif" {
		t.Errorf("AVIF should win over WebP, got %q", a)
	}

	if got := acceptKey("image/webp"); got != "image/webp" {
		t.Errorf("webp-only client: got %q", got)
	}
	if got := acceptKey("*/*"); got != "source" {
		t.Errorf("a client with no modern format should key as source, got %q", got)
	}
}
