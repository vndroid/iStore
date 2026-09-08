package ossprocess

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/vips/color"
)

// ResizeMode is OSS's `m_` parameter.
type ResizeMode int

const (
	// ModeLfit scales so the result fits inside w x h. OSS's default.
	ModeLfit ResizeMode = iota
	// ModeMfit scales so the result covers w x h, without cropping. One
	// dimension therefore overflows.
	ModeMfit
	// ModeFill covers w x h and centre-crops to exactly w x h.
	ModeFill
	// ModePad fits inside w x h and pads to exactly w x h with Color.
	ModePad
	// ModeFixed forces exactly w x h, ignoring the source aspect ratio.
	ModeFixed
)

var resizeModes = map[string]ResizeMode{
	"lfit":  ModeLfit,
	"mfit":  ModeMfit,
	"fill":  ModeFill,
	"pad":   ModePad,
	"fixed": ModeFixed,
}

func (m ResizeMode) String() string {
	for name, v := range resizeModes {
		if v == m {
			return name
		}
	}
	return "lfit"
}

// Resize is a parsed `image/resize,...` action.
//
// It is deliberately a description, not a set of pipeline options: three of
// OSS's parameters cannot be turned into a scale factor without knowing how big
// the source is.
//
//   - m_mfit and s_ (shortest side) constrain the *smaller* output dimension,
//     which depends on the source aspect ratio.
//   - limit_1, OSS's default, means "if the target is bigger than the source,
//     return the source untouched" — a comparison against the source.
//
// Resolve does that arithmetic once the source dimensions are known.
type Resize struct {
	Mode    ResizeMode
	W, H    int
	Long    int // l_: longest side
	Short   int // s_: shortest side
	Percent int // p_: 1..1000, percent of the original
	Limit   bool
	Color   color.RGB
}

// Target is a resolved resize: concrete pixel dimensions plus how to reach them.
type Target struct {
	W, H  int
	Type  processing.ResizeType
	Pad   bool      // extend to exactly W x H after fitting
	Color color.RGB // pad colour
	// Noop is set when limit_1 applies: the requested size is larger than the
	// source, so OSS returns the source unchanged.
	Noop bool
}

// parseResize reads the parameters of an `image/resize` action.
func parseResize(a Action) (*Resize, error) {
	r := &Resize{
		Mode:  ModeLfit,
		Limit: true,                              // OSS default: never enlarge
		Color: color.RGB{R: 255, G: 255, B: 255}, // OSS default pad colour is white
	}

	seen := map[string]bool{}
	for _, p := range a.Params {
		if p.Key == "" {
			return nil, fmt.Errorf("\"resize\" parameters need a key, got a bare %q", p.Value)
		}
		if seen[p.Key] {
			return nil, fmt.Errorf("\"resize\" parameter %q given twice", p.Key)
		}
		seen[p.Key] = true

		switch p.Key {
		case "m":
			m, ok := resizeModes[strings.ToLower(p.Value)]
			if !ok {
				return nil, fmt.Errorf("unsupported resize mode %q", p.Value)
			}
			r.Mode = m
		case "w":
			v, err := positive(p, 1, 1<<16)
			if err != nil {
				return nil, err
			}
			r.W = v
		case "h":
			v, err := positive(p, 1, 1<<16)
			if err != nil {
				return nil, err
			}
			r.H = v
		case "l":
			v, err := positive(p, 1, 1<<16)
			if err != nil {
				return nil, err
			}
			r.Long = v
		case "s":
			v, err := positive(p, 1, 1<<16)
			if err != nil {
				return nil, err
			}
			r.Short = v
		case "p":
			v, err := positive(p, 1, 1000)
			if err != nil {
				return nil, err
			}
			r.Percent = v
		case "limit":
			switch p.Value {
			case "0":
				r.Limit = false
			case "1":
				r.Limit = true
			default:
				return nil, fmt.Errorf("\"resize\" limit must be 0 or 1, got %q", p.Value)
			}
		case "color":
			c, err := parseHexColor(p.Value)
			if err != nil {
				return nil, err
			}
			r.Color = c
		default:
			return nil, fmt.Errorf("unsupported \"resize\" parameter %q", p.Key)
		}
	}

	// OSS rejects mixing the box parameters with the single-side ones.
	hasBox := r.W > 0 || r.H > 0
	hasSide := r.Long > 0 || r.Short > 0
	switch {
	case r.Percent > 0 && (hasBox || hasSide):
		return nil, fmt.Errorf("\"resize\" p cannot be combined with w/h/l/s")
	case r.Long > 0 && r.Short > 0:
		return nil, fmt.Errorf("\"resize\" l and s cannot be combined")
	case hasBox && hasSide:
		return nil, fmt.Errorf("\"resize\" w/h cannot be combined with l/s")
	case !hasBox && !hasSide && r.Percent == 0:
		return nil, fmt.Errorf("\"resize\" needs at least one of w, h, l, s or p")
	}

	return r, nil
}

func positive(p Param, lo, hi int) (int, error) {
	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("\"resize\" %s must be a number, got %q", p.Key, p.Value)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("\"resize\" %s must be between %d and %d, got %d", p.Key, lo, hi, v)
	}
	return v, nil
}

func parseHexColor(s string) (color.RGB, error) {
	if len(s) != 6 {
		return color.RGB{}, fmt.Errorf("colour must be 6 hex digits, got %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGB{}, fmt.Errorf("colour must be 6 hex digits, got %q", s)
	}
	return color.RGB{R: byte(v >> 16), G: byte(v >> 8), B: byte(v)}, nil
}

// Resolve turns the description into concrete dimensions for a source of
// srcW x srcH.
func (r *Resize) Resolve(srcW, srcH int) Target {
	if srcW <= 0 || srcH <= 0 {
		return Target{Noop: true}
	}

	// p_ is a straight percentage of the source and ignores mode entirely.
	if r.Percent > 0 {
		t := Target{
			W:    maxInt(1, srcW*r.Percent/100),
			H:    maxInt(1, srcH*r.Percent/100),
			Type: processing.ResizeForce,
		}
		if r.Limit && r.Percent > 100 {
			t.Noop = true
		}
		return t
	}

	w, h, mode := r.W, r.H, r.Mode

	// l_ and s_ are shorthands for a square box with a particular mode:
	// the longest side fitting inside n is lfit on n x n; the shortest side
	// reaching n is mfit on n x n.
	switch {
	case r.Long > 0:
		w, h, mode = r.Long, r.Long, ModeLfit
	case r.Short > 0:
		w, h, mode = r.Short, r.Short, ModeMfit
	}

	// A single given side means "scale proportionally to that side". Every mode
	// degenerates to the same thing, so compute the other side here and let the
	// force path below carry it.
	if w == 0 || h == 0 {
		if w == 0 {
			w = maxInt(1, srcW*h/srcH)
		} else {
			h = maxInt(1, srcH*w/srcW)
		}
		t := Target{W: w, H: h, Type: processing.ResizeForce}
		t.Noop = r.Limit && (w > srcW || h > srcH)
		return t
	}

	t := Target{W: w, H: h, Color: r.Color}

	switch mode {
	case ModeFixed:
		t.Type = processing.ResizeForce
	case ModeFill:
		t.Type = processing.ResizeFill
	case ModePad:
		t.Type = processing.ResizeFit
		t.Pad = true
	case ModeMfit:
		// Cover the box without cropping. imgproxy's ResizeFill scales exactly
		// this way but then crops to w x h, so instead compute the covering size
		// here and force it. The aspect ratio is preserved because both sides
		// come from one scale factor.
		scale := maxFloat(float64(w)/float64(srcW), float64(h)/float64(srcH))
		t.W = maxInt(1, int(float64(srcW)*scale+0.5))
		t.H = maxInt(1, int(float64(srcH)*scale+0.5))
		t.Type = processing.ResizeForce
	default: // ModeLfit
		t.Type = processing.ResizeFit
	}

	// limit_1 (the default): if honouring the request would enlarge the source,
	// OSS returns the source untouched rather than upscaling.
	if r.Limit && wouldEnlarge(mode, srcW, srcH, w, h) {
		t.Noop = true
	}

	return t
}

// wouldEnlarge reports whether reaching w x h from srcW x srcH in this mode
// requires scaling up.
func wouldEnlarge(mode ResizeMode, srcW, srcH, w, h int) bool {
	switch mode {
	case ModeFixed:
		return w > srcW || h > srcH
	case ModeFill, ModeMfit:
		// Both scale by the larger ratio, so enlargement happens when either
		// target side exceeds its source side.
		return float64(w)/float64(srcW) > 1 || float64(h)/float64(srcH) > 1
	default: // lfit, pad — scale by the smaller ratio
		return float64(w)/float64(srcW) > 1 && float64(h)/float64(srcH) > 1
	}
}

// apply writes the resolved target into the pipeline options.
func (t Target) apply(o *options.Options) {
	if t.Noop {
		return
	}
	o.Set(keys.Width, t.W)
	o.Set(keys.Height, t.H)
	o.Set(keys.ResizingType, t.Type)
	// The pipeline refuses to upscale unless told otherwise, and by this point
	// every OSS limit rule has already been applied above.
	o.Set(keys.Enlarge, true)

	if t.Pad {
		o.Set(keys.ExtendEnabled, true)
		o.Set(keys.Background, t.Color)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
