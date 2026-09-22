package httpserver

import (
	"encoding/binary"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// brokenSources are well-formed enough to be identified by their magic bytes
// and broken enough that libvips refuses them. Each used to come back 500
// "Internal error" from a transform — the error classifier only recognised a
// few loader prefixes, and only on the first line of libvips' error buffer —
// while image/info on the same file said 422.
func brokenSources() map[string][]byte {
	bmp := make([]byte, 54+16)
	copy(bmp, "BM")
	binary.LittleEndian.PutUint32(bmp[2:], uint32(len(bmp)))
	binary.LittleEndian.PutUint32(bmp[10:], 54)
	binary.LittleEndian.PutUint32(bmp[14:], 40)
	binary.LittleEndian.PutUint32(bmp[18:], 2)
	binary.LittleEndian.PutUint32(bmp[22:], 2)
	binary.LittleEndian.PutUint16(bmp[26:], 2) // planes must be 1
	binary.LittleEndian.PutUint16(bmp[28:], 24)

	// One directory entry whose image data (128 KiB at offset 22) is not there.
	ico := make([]byte, 86)
	copy(ico, []byte{0, 0, 1, 0, 1, 0, 32, 32, 0, 0, 1, 0, 32, 0})
	binary.LittleEndian.PutUint32(ico[14:], 0x20000)
	binary.LittleEndian.PutUint32(ico[18:], 22)

	webp := []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00garbage-not-a-vp8-frame!")

	tiff := append([]byte("II*\x00\x08\x00\x00\x00"), make([]byte, 24)...)
	binary.LittleEndian.PutUint16(tiff[8:], 0xffff) // absurd entry count

	avif := append([]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"), make([]byte, 64)...)

	return map[string][]byte{
		"broken.bmp": bmp, "broken.ico": ico, "broken.webp": webp,
		"broken.tiff": tiff, "broken.avif": avif,
	}
}

func TestBrokenSourceIsUnprocessableNotInternal(t *testing.T) {
	dir := t.TempDir()
	for name, b := range brokenSources() {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := newFullServer(t, dir, nil)
	for name := range brokenSources() {
		for _, chain := range []string{"image/resize,w_10", "image/format,png"} {
			if got := status(s, http.MethodGet, "/"+name+"?x-oss-process="+chain, ""); got != http.StatusUnprocessableEntity {
				t.Errorf("%s %s: status %d, want 422", name, chain, got)
			}
		}
	}
}
