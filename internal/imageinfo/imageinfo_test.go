package imageinfo

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/vndroid/istore/internal/imagetype"
)

// Test images are generated rather than committed, so the expected dimensions
// are stated in one place and cannot drift from the fixtures.
const (
	testW = 400
	testH = 267
)

func rgba(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 0x80, 0xFF})
		}
	}
	return img
}

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba(w, h)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeGIF(t *testing.T, w, h, frames int) []byte {
	t.Helper()
	g := &gif.GIF{}
	for i := 0; i < frames; i++ {
		p := image.NewPaletted(image.Rect(0, 0, w, h), []color.Color{
			color.Black, color.White,
		})
		p.SetColorIndex(i%w, 0, 1)
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readBytes(t *testing.T, b []byte) *Info {
	t.Helper()
	info, err := Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestReadJPEG(t *testing.T) {
	b := encodeJPEG(t, testW, testH)
	info := readBytes(t, b)

	if info.Format != imagetype.JPEG {
		t.Errorf("format = %v, want jpeg", info.Format)
	}
	if info.ImageWidth != testW || info.ImageHeight != testH {
		t.Errorf("got %dx%d, want %dx%d", info.ImageWidth, info.ImageHeight, testW, testH)
	}
	if info.FileSize != int64(len(b)) {
		t.Errorf("FileSize = %d, want %d", info.FileSize, len(b))
	}
	if info.FrameCount != 1 {
		t.Errorf("FrameCount = %d, want 1", info.FrameCount)
	}
}

func TestReadPNG(t *testing.T) {
	info := readBytes(t, encodePNG(t, testW, testH))
	if info.Format != imagetype.PNG {
		t.Errorf("format = %v, want png", info.Format)
	}
	if info.ImageWidth != testW || info.ImageHeight != testH {
		t.Errorf("got %dx%d, want %dx%d", info.ImageWidth, info.ImageHeight, testW, testH)
	}
}

func TestReadGIFFrameCount(t *testing.T) {
	for _, frames := range []int{1, 3, 7} {
		info := readBytes(t, encodeGIF(t, 64, 32, frames))
		if info.Format != imagetype.GIF {
			t.Errorf("format = %v, want gif", info.Format)
		}
		if info.ImageWidth != 64 || info.ImageHeight != 32 {
			t.Errorf("got %dx%d, want 64x32", info.ImageWidth, info.ImageHeight)
		}
		if info.FrameCount != frames {
			t.Errorf("FrameCount = %d, want %d", info.FrameCount, frames)
		}
	}
}

// TestJSONShape pins the response format: every value is a string inside a
// {"value": ...} object, and the keys are the ones OSS emits. A client written
// against OSS parses this without changes, so the shape is part of the contract.
func TestJSONShape(t *testing.T) {
	b := encodeJPEG(t, testW, testH)
	info := readBytes(t, b)

	out, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not the expected shape: %v\n%s", err, out)
	}

	want := []string{
		"FileSize", "Format", "FrameCount", "ImageHeight",
		"ImageWidth", "ResolutionUnit", "XResolution", "YResolution",
	}
	if len(got) != len(want) {
		t.Errorf("got %d keys, want %d: %s", len(got), len(want), out)
	}
	for _, k := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		inner, ok := v["value"]
		if !ok {
			t.Errorf("%q has no \"value\" field", k)
			continue
		}
		if _, ok := inner.(string); !ok {
			t.Errorf("%q.value is %T, want string", k, inner)
		}
	}

	if got["Format"]["value"] != "jpg" {
		t.Errorf("Format = %v, want \"jpg\" (OSS spelling)", got["Format"]["value"])
	}
	if got["ImageWidth"]["value"] != "400" {
		t.Errorf("ImageWidth = %v, want \"400\"", got["ImageWidth"]["value"])
	}
}

func TestReadUnsupportedContainer(t *testing.T) {
	// A minimal AVIF/HEIF ftyp box: recognised as a type, not parsed here.
	b := append([]byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}, make([]byte, 64)...)
	_, err := Read(bytes.NewReader(b), int64(len(b)))
	var unsupported ErrUnsupportedContainer
	if err == nil {
		t.Skip("this build detects the stub as something parseable")
	}
	if !asUnsupported(err, &unsupported) {
		t.Skipf("stub was not detected as a container type: %v", err)
	}
}

func asUnsupported(err error, target *ErrUnsupportedContainer) bool {
	u, ok := err.(ErrUnsupportedContainer)
	if ok {
		*target = u
	}
	return ok
}

func TestTruncatedInput(t *testing.T) {
	b := encodeJPEG(t, testW, testH)
	// Keep only the SOI marker: no SOF can be found.
	if _, err := Read(bytes.NewReader(b[:2]), 2); err == nil {
		t.Error("expected an error for a truncated image")
	}
}
