package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/vndroid/istore/internal/ossprocess"
)

// OSS allows 1–63 characters from this set for user-defined style names.
var styleName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// loadStyles takes an immutable snapshot. In particular, a running server
// never combines a new style definition with an old cached result.
func loadStyles(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load styles %q: %w", path, err)
	}
	var styles map[string]string
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&styles); err != nil {
		return nil, fmt.Errorf("load styles %q: expected a JSON object of names to chains: %v", path, err)
	}
	if styles == nil {
		return nil, fmt.Errorf("load styles %q: expected a JSON object of names to chains", path)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("load styles %q: trailing JSON data", path)
	}
	for name, raw := range styles {
		if !styleName.MatchString(name) {
			return nil, fmt.Errorf("style %q: name must be 1–63 ASCII letters, digits, _, - or .", name)
		}
		if strings.HasPrefix(raw, "style/") {
			return nil, fmt.Errorf("style %q: recursive styles are not allowed", name)
		}
		chain, err := ossprocess.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("style %q: %w", name, err)
		}
		if chain.IsEmpty() {
			return nil, fmt.Errorf("style %q: empty processing chain", name)
		}
		if err := chain.Validate(); err != nil {
			return nil, fmt.Errorf("style %q: %w", name, err)
		}
		if err := chain.CheckEncoders(); err != nil {
			return nil, fmt.Errorf("style %q: %w", name, err)
		}
	}
	return styles, nil
}

func expandStyle(raw string, styles map[string]string) (string, error) {
	if !strings.HasPrefix(raw, "style/") {
		return raw, nil
	}
	name := strings.TrimPrefix(raw, "style/")
	if !styleName.MatchString(name) {
		return "", fmt.Errorf("style must be used alone as style/<name>")
	}
	effective, ok := styles[name]
	if !ok {
		return "", fmt.Errorf("unknown style %q", name)
	}
	return effective, nil
}
