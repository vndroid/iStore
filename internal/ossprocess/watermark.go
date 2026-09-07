package ossprocess

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/processing"
	"github.com/kane/istore/internal/vips/color"
)

// Options the parser leaves for iStore's own watermark provider, which builds
// the watermark image the pipeline then composites.
//
// The pipeline's watermark comes from an auximageprovider.Provider, which
// imgproxy configures once at startup. OSS names a different watermark per
// request — a different object key, or a line of text to render — so the
// provider reads these instead of a fixed config. They are prefixed to keep them
// clear of the imgproxy key space in the same bag.
const (
	KeyWatermarkPath   = "istore.watermark_path"
	KeyWatermarkText   = "istore.watermark_text"
	KeyWatermarkFont   = "istore.watermark_font"
	KeyWatermarkColor  = "istore.watermark_color"
	KeyWatermarkSize   = "istore.watermark_size"
	KeyWatermarkShadow = "istore.watermark_shadow"
	KeyWatermarkRotate = "istore.watermark_rotate"

	// Only meaningful when both an image and text were given.
	KeyWatermarkOrder    = "istore.watermark_order"
	KeyWatermarkAlign    = "istore.watermark_align"
	KeyWatermarkInterval = "istore.watermark_interval"
)

// ossWatermarkGravities maps OSS's `g_` for watermarks. Same anchor names as
// crop, but the pipeline uses a different option key for watermark position.
var ossWatermarkGravities = ossGravities

// Watermark is a parsed `image/watermark` action.
//
//	watermark,image_<base64url object key>              an image watermark
//	watermark,text_<base64url text>                     a text watermark
//	watermark,image_...,text_...,order_1,align_1        both, side by side
//
// Every parameter OSS documents is accepted. Two things about the rendering are
// iStore's own and cannot match OSS byte for byte: which font a name resolves to
// (that is the host's fontconfig, not ours), and the geometry of the drop
// shadow — OSS's `shadow_` gives its transparency and nothing about its offset
// or blur, so those are scaled from the font size here.
type Watermark struct {
	// Image watermark.
	Path  string
	Scale float64 // P_/100; 0 means the watermark's own size

	// Text watermark.
	Text   string
	Font   string
	Color  color.RGB
	Size   int
	Shadow float64 // 0..1
	Rotate int

	// Shared.
	Opacity float64
	Gravity processing.GravityType
	X, Y    int

	// Tiling. Fill replaces the anchor entirely: the watermark is repeated
	// across the whole image with PadX/PadY between copies.
	Fill       bool
	PadX, PadY int

	// Layout when both an image and text are present.
	Order    int // 0: image first, 1: text first
	Align    int // 0: top, 1: middle, 2: bottom
	Interval int
}

// Watermark defaults, all from OSS's own table.
const (
	watermarkDefaultSize   = 40
	watermarkDefaultOffset = 10
	watermarkDefaultAlign  = 2 // bottom
)

// watermarkDefaultFont is what OSS calls wqy-zenhei. Resolving that exact face
// would mean requiring a specific font package on every host; "sans" is
// whatever fontconfig considers the system sans-serif, which on any machine set
// up to display Chinese covers the same text.
const watermarkDefaultFont = "sans"

// ossFontNames maps OSS's font identifiers onto families fontconfig can be
// expected to resolve. An unlisted name is passed through unchanged, so a
// deployment can name a font it has actually installed.
var ossFontNames = map[string]string{
	"wqy-zenhei":        "WenQuanYi Zen Hei,sans",
	"wqy-microhei":      "WenQuanYi Micro Hei,sans",
	"fangzhengshusong":  "FZShuSong,serif",
	"fangzhengkaiti":    "FZKai,serif",
	"fangzhengheiti":    "FZHei,sans",
	"fangzhengfangsong": "FZFangSong,serif",
	"droidsansfallback": "Droid Sans Fallback,sans",
	"georgia":           "Georgia,serif",
	"times new roman":   "Times New Roman,serif",
}

func parseWatermark(a Action) (*Watermark, error) {
	w := &Watermark{
		Opacity: 1.0,
		Gravity: processing.GravitySouthEast, // OSS's default is bottom-right
		X:       watermarkDefaultOffset,
		Y:       watermarkDefaultOffset,
		Font:    watermarkDefaultFont,
		Size:    watermarkDefaultSize,
		Color:   color.Black,
		Align:   watermarkDefaultAlign,
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

		var err error

		switch p.Key {
		case "image":
			w.Path, err = decodeOSSKey(p, "image")
		case "text":
			w.Text, err = decodeOSSKey(p, "text")
		case "type":
			var name string
			if name, err = decodeOSSKey(p, "type"); err == nil {
				w.Font = resolveFont(name)
			}
		case "color":
			w.Color, err = parseHexColor(p.Value)
			if err != nil {
				err = fmt.Errorf("\"watermark\" color: %w", err)
			}
		case "size":
			w.Size, err = watermarkInt(p, 1, 1000)
		case "shadow":
			var v int
			if v, err = watermarkInt(p, 0, 100); err == nil {
				w.Shadow = float64(v) / 100
			}
		case "rotate":
			w.Rotate, err = watermarkInt(p, 0, 360)
		case "t":
			var v int
			if v, err = watermarkInt(p, 0, 100); err == nil {
				w.Opacity = float64(v) / 100
			}
		case "g":
			g, ok := ossWatermarkGravities[strings.ToLower(p.Value)]
			if !ok {
				return nil, fmt.Errorf("unsupported watermark anchor %q", p.Value)
			}
			w.Gravity = g
		case "x":
			w.X, err = watermarkInt(p, 0, 4096)
		case "y":
			w.Y, err = watermarkInt(p, 0, 4096)
		case "voffset":
			// The distance from the centre line, so it is signed and it
			// replaces y_ rather than adding to it.
			w.Y, err = watermarkInt(p, -1000, 1000)
		case "P":
			var v int
			if v, err = watermarkInt(p, 1, 100); err == nil {
				w.Scale = float64(v) / 100
			}
		case "fill":
			w.Fill, err = watermarkFlag(p)
		case "padx":
			w.PadX, err = watermarkInt(p, 0, 4096)
		case "pady":
			w.PadY, err = watermarkInt(p, 0, 4096)
		case "order":
			w.Order, err = watermarkInt(p, 0, 1)
		case "align":
			w.Align, err = watermarkInt(p, 0, 2)
		case "interval":
			w.Interval, err = watermarkInt(p, 0, 1000)
		default:
			return nil, fmt.Errorf("unsupported \"watermark\" parameter %q", p.Key)
		}

		if err != nil {
			return nil, err
		}
	}

	return w, w.validate(seen)
}

// validate rejects combinations that cannot all be honoured at once. Each of
// these would otherwise mean quietly ignoring something the caller asked for,
// which is the failure mode this package exists to avoid.
func (w *Watermark) validate(seen map[string]bool) error {
	if w.Path == "" && w.Text == "" {
		return fmt.Errorf("\"watermark\" needs image_<base64url of an object key> or text_<base64url of the text>")
	}

	if seen["y"] && seen["voffset"] {
		return fmt.Errorf("\"watermark\" takes y or voffset, not both: both are the vertical offset")
	}

	if w.Fill {
		for _, k := range []string{"g", "x", "y", "voffset"} {
			if seen[k] {
				return fmt.Errorf("\"watermark\" %s has no meaning with fill_1, which tiles the whole image", k)
			}
		}
	} else if seen["padx"] || seen["pady"] {
		return fmt.Errorf("\"watermark\" padx/pady are the gaps between tiles and need fill_1")
	}

	if w.Path == "" {
		if seen["P"] {
			return fmt.Errorf("\"watermark\" P scales an image watermark and needs image_")
		}
	}

	if w.Text == "" {
		for _, k := range []string{"type", "color", "size", "shadow", "rotate"} {
			if seen[k] {
				return fmt.Errorf("\"watermark\" %s describes a text watermark and needs text_", k)
			}
		}
	}

	if w.Path == "" || w.Text == "" {
		for _, k := range []string{"order", "align", "interval"} {
			if seen[k] {
				return fmt.Errorf("\"watermark\" %s lays out an image and text together and needs both", k)
			}
		}
	}

	return nil
}

// resolveFont turns OSS's font identifier into a Pango font family list.
func resolveFont(name string) string {
	if f, ok := ossFontNames[strings.ToLower(strings.TrimSpace(name))]; ok {
		return f
	}
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return watermarkDefaultFont
}

func decodeOSSKey(p Param, what string) (string, error) {
	s, err := decodeOSSBase64(p.Value)
	if err != nil {
		return "", fmt.Errorf("\"watermark\" %s must be base64url: %w", what, err)
	}
	if s == "" {
		return "", fmt.Errorf("\"watermark\" %s decoded to nothing", what)
	}
	return s, nil
}

func watermarkInt(p Param, lo, hi int) (int, error) {
	v, err := strconv.Atoi(p.Value)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("\"watermark\" %s must be between %d and %d, got %q", p.Key, lo, hi, p.Value)
	}
	return v, nil
}

func watermarkFlag(p Param) (bool, error) {
	switch p.Value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("\"watermark\" %s must be 0 or 1, got %q", p.Key, p.Value)
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
	if w.Path != "" {
		o.Set(KeyWatermarkPath, w.Path)
	}
	if w.Text != "" {
		o.Set(KeyWatermarkText, w.Text)
		o.Set(KeyWatermarkFont, w.Font)
		o.Set(KeyWatermarkColor, w.Color)
		o.Set(KeyWatermarkSize, w.Size)
		o.Set(KeyWatermarkShadow, w.Shadow)
		o.Set(KeyWatermarkRotate, w.Rotate)
	}
	if w.Path != "" && w.Text != "" {
		o.Set(KeyWatermarkOrder, w.Order)
		o.Set(KeyWatermarkAlign, w.Align)
		o.Set(KeyWatermarkInterval, w.Interval)
	}

	o.Set(keys.WatermarkOpacity, w.Opacity)

	if w.Scale > 0 {
		o.Set(keys.WatermarkScale, w.Scale)
	}

	if w.Fill {
		// The pipeline spells tiling as a gravity, and reads the offsets as the
		// padding around each tile rather than a position.
		o.Set(keys.WatermarkPosition, processing.GravityReplicate)
		o.Set(keys.WatermarkXOffset, float64(w.PadX))
		o.Set(keys.WatermarkYOffset, float64(w.PadY))
		return
	}

	o.Set(keys.WatermarkPosition, w.Gravity)
	o.Set(keys.WatermarkXOffset, float64(w.X))
	o.Set(keys.WatermarkYOffset, float64(w.Y))
}
