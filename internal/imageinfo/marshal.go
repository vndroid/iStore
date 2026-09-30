package imageinfo

import (
	"bytes"
	"encoding/json"
	"io"
)

// marshalOSS encodes v without escaping HTML characters and without the
// trailing newline encoder.Encode adds, so the body matches OSS byte for byte.
func marshalOSS(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// newSeekableReader wraps a byte slice for imagetype.Detect, which wants a
// reader it can peek on.
func newSeekableReader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }
