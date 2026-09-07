package ossprocess

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/processing"
)

// KeyWatermarkPath is where the parser leaves the watermark's object key for the
// server's image provider to pick up.
//
// The pipeline's watermark comes from an auximageprovider.Provider, which
// imgproxy configures once at startup. OSS names a different watermark per
// request, so iStore's provider reads this option instead of a fixed config.
// The name is prefixed to keep it clear of the imgproxy key space in the same
// bag.
const KeyWatermarkPath = "istore.watermark_path"

// ossWatermarkGravities maps OSS's `g_` for watermarks. Same anchor names as
// crop, but the pipeline uses a different option key for watermark position.
var ossWatermarkGravities = ossGravities

// Watermark is a parsed `image/watermark` action.
//
// Only image watermarks are supported. OSS also accepts `text_<base64>`, which
// needs a text renderer: libvips has one (vips_text, via Pango), but imgproxy's
// binding does not expose it, so a text watermark is refused rather than
// silently dropped.
type Watermark struct {
	Path    string // decoded object key, relative to the serving root
	Opacity float64
	Gravity processing.GravityType
	X, Y    int
}

func parseWatermark(a Action) (*Watermark, error) {
	w := &Watermark{
		Opacity: 1.0,
		Gravity: processing.GravitySouthEast, // OSS's default is bottom-right
	}

	seen := map[string]bool{}
	for _, p := range a.Params {
		if p.Key == "" {
			return nil, fmt.Errorf("\"watermark\" parameters need a key, got a bare %q", p.Value)
		}
		if seen[p.Key] {
			return nil, fmt.Errorf("\"watermark\" parameter %q given twice", p.Key)
		}
		seen[p.Key] = true

		switch p.Key {
		case "image":
			path, err := decodeOSSBase64(p.Value)
			if err != nil {
				return nil, fmt.Errorf("\"watermark\" image must be base64url of the object key: %w", err)
			}
			if path == "" {
				return nil, fmt.Errorf("\"watermark\" image decoded to an empty key")
			}
			w.Path = path
		case "text":
			return nil, fmt.Errorf("text watermarks are not supported; use watermark,image_<base64url of an object key>")
		case "t":
			v, err := strconv.Atoi(p.Value)
			if err != nil || v < 0 || v > 100 {
				return nil, fmt.Errorf("\"watermark\" t (opacity) must be between 0 and 100, got %q", p.Value)
			}
			w.Opacity = float64(v) / 100
		case "g":
			g, ok := ossWatermarkGravities[strings.ToLower(p.Value)]
			if !ok {
				return nil, fmt.Errorf("unsupported watermark anchor %q", p.Value)
			}
			w.Gravity = g
		case "x":
			v, err := watermarkOffset(p)
			if err != nil {
				return nil, err
			}
			w.X = v
		case "y":
			v, err := watermarkOffset(p)
			if err != nil {
				return nil, err
			}
			w.Y = v
		default:
			return nil, fmt.Errorf("unsupported \"watermark\" parameter %q", p.Key)
		}
	}

	if w.Path == "" {
		return nil, fmt.Errorf("\"watermark\" needs image_<base64url of an object key>")
	}

	return w, nil
}

func watermarkOffset(p Param) (int, error) {
	v, err := strconv.Atoi(p.Value)
	if err != nil || v < 0 || v > 4096 {
		return 0, fmt.Errorf("\"watermark\" %s must be between 0 and 4096, got %q", p.Key, p.Value)
	}
	return v, nil
}

// decodeOSSBase64 decodes OSS's URL-safe, unpadded base64.
//
// OSS documents the encoding as "URL-safe base64 with = removed", but real URLs
// turn up with padding kept and occasionally with the standard +/ alphabet, so
// all four combinations are accepted.
func decodeOSSBase64(s string) (string, error) {
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.RawStdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("not valid base64: %q", s)
}

func (w *Watermark) apply(o *options.Options) {
	o.Set(KeyWatermarkPath, w.Path)
	o.Set(keys.WatermarkOpacity, w.Opacity)
	o.Set(keys.WatermarkPosition, w.Gravity)
	o.Set(keys.WatermarkXOffset, float64(w.X))
	o.Set(keys.WatermarkYOffset, float64(w.Y))
}

// WatermarkPath returns the watermark object key a chain asked for, if any.
// The server uses it to decide whether to attach an image provider at all.
func (c *Chain) WatermarkPath() string {
	for _, a := range c.Actions {
		if a.Name != "watermark" {
			continue
		}
		if w, err := parseWatermark(a); err == nil {
			return w.Path
		}
	}
	return ""
}
