package imageinfo

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/kane/istore/internal/imagetype"
)

// FrameCountKnown is the contract between this package and the HTTP layer: false
// means "FrameCount is a floor, go and find the real one". Getting it wrong in
// either direction is expensive — false when the count is exact makes every such
// request pay a libvips load, true when it is a floor serves a number that is
// quietly incorrect.

func TestFrameCountKnownForShortGIF(t *testing.T) {
	for _, frames := range []int{1, 3, 7} {
		info := readBytes(t, encodeGIF(t, 64, 32, frames))

		if !info.FrameCountKnown {
			t.Errorf("%d-frame GIF: FrameCountKnown = false, want true — the whole "+
				"block structure fits in the header window, so the count is exact", frames)
		}
		if info.FrameCount != frames {
			t.Errorf("FrameCount = %d, want %d", info.FrameCount, frames)
		}
	}
}

// A GIF whose blocks run past the 64 KiB window counts what it can see and says
// so. Before this, the floor was returned as though it were the answer: a
// 200-frame animation reported 183 with nothing to indicate otherwise.
func TestFrameCountUnknownForLongGIF(t *testing.T) {
	const frames = 400

	b := encodeGIF(t, 200, 200, frames)
	if len(b) <= HeaderBytes {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d-byte window", len(b), HeaderBytes)
	}

	info := readBytes(t, b)

	if info.FrameCountKnown {
		t.Error("FrameCountKnown = true for a GIF longer than the header window")
	}
	if info.FrameCount >= frames {
		t.Errorf("FrameCount = %d; the header walk cannot have seen all %d frames", info.FrameCount, frames)
	}
	if info.FrameCount < 1 {
		t.Errorf("FrameCount = %d, want at least the floor of 1", info.FrameCount)
	}
}

// animatedWebP is a VP8X WebP with the animation flag set and no ANMF chunks in
// the header window — the shape whose frame count this package cannot settle.
func animatedWebP(t *testing.T, w, h int) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	buf.Write(make([]byte, 4)) // size, patched below
	buf.WriteString("WEBP")

	buf.WriteString("VP8X")
	binary.Write(&buf, binary.LittleEndian, uint32(10))
	buf.WriteByte(0x02) // animation flag
	buf.Write(make([]byte, 3))
	buf.Write([]byte{
		byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16),
		byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16),
	})

	b := buf.Bytes()
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

func TestFrameCountUnknownForAnimatedWebP(t *testing.T) {
	b := animatedWebP(t, 120, 80)

	info, err := Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}

	if info.Format != imagetype.WEBP {
		t.Fatalf("format = %v, want webp", info.Format)
	}
	if info.ImageWidth != 120 || info.ImageHeight != 80 {
		t.Errorf("got %dx%d, want 120x80", info.ImageWidth, info.ImageHeight)
	}
	if info.FrameCountKnown {
		t.Error("FrameCountKnown = true for an animated WebP; the count is in ANMF chunks")
	}
	// The floor used to be 0, which is worse than wrong: it reads as "no frames"
	// for a file that certainly has some.
	if info.FrameCount < 1 {
		t.Errorf("FrameCount = %d, want at least 1", info.FrameCount)
	}
}

func TestFrameCountKnownForStillWebP(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	buf.Write(make([]byte, 4))
	buf.WriteString("WEBP")
	buf.WriteString("VP8L")
	// The payload is padded to keep the whole file past readWebP's 30-byte
	// minimum; only the first five bytes carry anything.
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	buf.WriteByte(0x2F) // VP8L signature
	// 14 bits width-1, 14 bits height-1
	binary.Write(&buf, binary.LittleEndian, uint32((119)|(79<<14)))
	buf.Write(make([]byte, 11))
	b := buf.Bytes()
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))

	info, err := Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if !info.FrameCountKnown {
		t.Error("FrameCountKnown = false for a still WebP")
	}
	if info.FrameCount != 1 {
		t.Errorf("FrameCount = %d, want 1", info.FrameCount)
	}
}

func TestFrameCountKnownForJPEG(t *testing.T) {
	info := readBytes(t, encodeJPEG(t, testW, testH))
	if !info.FrameCountKnown {
		t.Error("FrameCountKnown = false for a JPEG; a JPEG has exactly one frame")
	}
}

// FrameCountKnown must never reach the response body: OSS has no such field.
func TestFrameCountKnownIsNotMarshalled(t *testing.T) {
	info := readBytes(t, encodeJPEG(t, testW, testH))

	out, err := info.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("FrameCountKnown")) {
		t.Errorf("FrameCountKnown leaked into the response: %s", out)
	}
}
