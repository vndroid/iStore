package httpserver

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/processing"
)

func TestTooLarge(t *testing.T) {
	tests := []struct {
		name  string
		limit int64
		size  int64
		want  bool
	}{
		{"under", 100, 99, false},
		{"exactly at the limit", 100, 100, false},
		{"over", 100, 101, true},
		{"no limit configured", 0, 1 << 40, false},
		{"negative limit is no limit", -1, 1 << 40, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{cfg: Config{MaxSourceBytes: tt.limit}}
			if got := s.tooLarge(tt.size); got != tt.want {
				t.Errorf("tooLarge(%d) with limit %d = %v, want %v", tt.size, tt.limit, got, tt.want)
			}
		})
	}
}

func TestErrSourceTooLargeStatus(t *testing.T) {
	err := errSourceTooLarge{size: 200, limit: 100}

	if err.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Errorf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusRequestEntityTooLarge)
	}
	if err.Error() == "" {
		t.Error("Error() is empty")
	}
}

// bmpFile writes a BMP of the given total size. BMP is a container imageinfo has
// no header walk for, so an info request for one takes the libvips fallback —
// the path that reads the file whole, and therefore the path the size limit has
// to cover.
func bmpFile(t *testing.T, dir, name string, size int) string {
	t.Helper()

	b := make([]byte, size)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], uint32(size))
	binary.LittleEndian.PutUint32(b[10:], 54) // pixel data offset
	binary.LittleEndian.PutUint32(b[14:], 40) // DIB header size
	binary.LittleEndian.PutUint32(b[18:], 16) // width
	binary.LittleEndian.PutUint32(b[22:], 16) // height
	binary.LittleEndian.PutUint16(b[26:], 1)  // planes
	binary.LittleEndian.PutUint16(b[28:], 24) // bits per pixel

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newLimitedServer(t *testing.T, root string, limit int64) *Server {
	t.Helper()

	s, err := New(
		Config{Root: root, MaxSourceBytes: limit},
		func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The info endpoint used to have no size limit at all, which made it the
// cheapest way to ask the process for memory: a transform of a 192 MB file was
// refused with 413 while info on the same file returned 200 after reading every
// byte of it. The limit now sits on the fallback, so this must be a 413 — and it
// must be one *before* any of the file is read, which is why this test can run
// with no libvips at all.
func TestInfoRefusesOversizedSource(t *testing.T) {
	dir := t.TempDir()
	bmpFile(t, dir, "big.bmp", 4096)

	s := newLimitedServer(t, dir, 1024)

	w := httptest.NewRecorder()
	s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/big.bmp", nil))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusRequestEntityTooLarge, w.Body)
	}
	if !strings.Contains(w.Body.String(), "SourceTooLarge") {
		t.Errorf("body = %s, want a SourceTooLarge envelope", w.Body)
	}
}

func TestAverageHueRefusesOversizedSource(t *testing.T) {
	dir := t.TempDir()
	bmpFile(t, dir, "big.bmp", 4096)

	s := newLimitedServer(t, dir, 1024)

	w := httptest.NewRecorder()
	s.serveAverageHue(w, httptest.NewRequest(http.MethodGet, "/big.bmp", nil))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusRequestEntityTooLarge, w.Body)
	}
}

// The limit belongs to the read, not to the request. A JPEG the header walk can
// answer from its first 64 KiB is served whatever its size — refusing it would
// be a regression dressed up as a safety check.
func TestInfoServesLargeSourceItCanAnswerFromTheHeader(t *testing.T) {
	dir := t.TempDir()

	b := testJPEG(t, 400, 267, 4096) // padded well past the limit below
	if err := os.WriteFile(filepath.Join(dir, "big.jpg"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	s := newLimitedServer(t, dir, 1024)

	w := httptest.NewRecorder()
	s.serveInfo(w, httptest.NewRequest(http.MethodGet, "/big.jpg", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"ImageWidth":{"value":"400"}`) {
		t.Errorf("body = %s, want the real dimensions", w.Body)
	}
}

// testJPEG encodes a JPEG and pads it past minSize with trailing bytes. Padding
// after EOI changes only the file's length, which is the point: the header walk
// still answers, and the size limit still sees a large file.
func testJPEG(t *testing.T, w, h, minSize int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xFF})
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}

	b := buf.Bytes()
	if len(b) < minSize {
		b = append(b, make([]byte, minSize-len(b))...)
	}
	return b
}
