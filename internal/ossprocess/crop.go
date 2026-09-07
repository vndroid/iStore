package ossprocess

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/processing"
)

// ossGravities maps OSS's `g_` anchor names onto the pipeline's gravity types.
//
// OSS's default anchor is nw (top-left), which is also what makes `x_`/`y_`
// behave as plain offsets from the top-left corner.
var ossGravities = map[string]processing.GravityType{
	"nw":     processing.GravityNorthWest,
	"north":  processing.GravityNorth,
	"ne":     processing.GravityNorthEast,
	"west":   processing.GravityWest,
	"center": processing.GravityCenter,
	"east":   processing.GravityEast,
	"sw":     processing.GravitySouthWest,
	"south":  processing.GravitySouth,
	"se":     processing.GravitySouthEast,
}

// Crop is a parsed `image/crop` action.
//
// Unlike resize, this needs nothing from the source: the pipeline clamps a crop
// larger than the image down to the image (imath.MinNonZero in cropImage), which
// is also what OSS does.
type Crop struct {
	W, H    int
	X, Y    int
	Gravity processing.GravityType
}

func parseCrop(a Action) (*Crop, error) {
	c := &Crop{Gravity: processing.GravityNorthWest}

	seen := map[string]bool{}
	for _, p := range a.Params {
		if p.Key == "" {
			return nil, fmt.Errorf("\"crop\" parameters need a key, got a bare %q", p.Value)
		}
		if seen[p.Key] {
			return nil, fmt.Errorf("\"crop\" parameter %q given twice", p.Key)
		}
		seen[p.Key] = true

		switch p.Key {
		case "w":
			v, err := cropInt(p, 1, 1<<16)
			if err != nil {
				return nil, err
			}
			c.W = v
		case "h":
			v, err := cropInt(p, 1, 1<<16)
			if err != nil {
				return nil, err
			}
			c.H = v
		case "x":
			v, err := cropInt(p, 0, 1<<16)
			if err != nil {
				return nil, err
			}
			c.X = v
		case "y":
			v, err := cropInt(p, 0, 1<<16)
			if err != nil {
				return nil, err
			}
			c.Y = v
		case "g":
			g, ok := ossGravities[strings.ToLower(p.Value)]
			if !ok {
				return nil, fmt.Errorf("unsupported crop anchor %q", p.Value)
			}
			c.Gravity = g
		default:
			return nil, fmt.Errorf("unsupported \"crop\" parameter %q", p.Key)
		}
	}

	if c.W == 0 && c.H == 0 {
		return nil, fmt.Errorf("\"crop\" needs w or h")
	}

	return c, nil
}

func cropInt(p Param, lo, hi int) (int, error) {
	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("\"crop\" %s must be a number, got %q", p.Key, p.Value)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("\"crop\" %s must be between %d and %d, got %d", p.Key, lo, hi, v)
	}
	return v, nil
}

func (c *Crop) apply(o *options.Options) {
	// CalcCropSize treats a value >= 1 as absolute pixels and anything smaller
	// as a fraction of the source, which is why these are written as whole
	// numbers and why the parser refuses a zero w or h.
	if c.W > 0 {
		o.Set(keys.CropWidth, float64(c.W))
	}
	if c.H > 0 {
		o.Set(keys.CropHeight, float64(c.H))
	}
	o.Set(keys.CropGravityType, c.Gravity)
	// calcPosition reads offsets with |v| >= 1 as pixels and smaller values as
	// fractions; OSS's x_/y_ are always whole pixels, and zero means zero under
	// either reading.
	o.Set(keys.CropGravityXOffset, float64(c.X))
	o.Set(keys.CropGravityYOffset, float64(c.Y))
}

// ---------------------------------------------------------------- rotate etc.

// parseRotate reads `image/rotate,<degrees>`.
//
// OSS accepts 0..360; libvips rotates losslessly only by multiples of 90, and
// the pipeline's Rotate option is defined in those terms, so anything else is
// refused rather than silently rounded.
func parseRotate(a Action) (int, error) {
	if len(a.Params) != 1 {
		return 0, fmt.Errorf("\"rotate\" takes exactly one value, e.g. rotate,90")
	}
	p := a.Params[0]
	if p.Key != "" {
		return 0, fmt.Errorf("\"rotate\" takes a bare value, got %q", p.Key)
	}
	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("\"rotate\" must be a number, got %q", p.Value)
	}
	if v < 0 || v > 360 {
		return 0, fmt.Errorf("\"rotate\" must be between 0 and 360, got %d", v)
	}
	if v%90 != 0 {
		return 0, fmt.Errorf("\"rotate\" must be a multiple of 90, got %d", v)
	}
	return v % 360, nil
}

// parseAutoOrient reads `image/auto-orient,<0|1>`: whether to apply the EXIF
// orientation tag. OSS's default is 1, and so is the pipeline's.
func parseAutoOrient(a Action) (bool, error) {
	if len(a.Params) != 1 {
		return false, fmt.Errorf("\"auto-orient\" takes exactly one value, 0 or 1")
	}
	p := a.Params[0]
	if p.Key != "" {
		return false, fmt.Errorf("\"auto-orient\" takes a bare value, got %q", p.Key)
	}
	switch p.Value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("\"auto-orient\" must be 0 or 1, got %q", p.Value)
}

// parseBlur reads `image/blur,r_<radius>,s_<sigma>`.
//
// OSS takes both a radius and a standard deviation. libvips' gaussian blur is
// parameterised by sigma alone and derives its own kernel radius, so `r_` is
// accepted for URL compatibility and does not affect the output. Both are
// required, matching OSS, so a URL that works there works here.
func parseBlur(a Action) (float64, error) {
	var sigma float64
	var haveR, haveS bool

	for _, p := range a.Params {
		switch p.Key {
		case "r":
			v, err := strconv.Atoi(p.Value)
			if err != nil || v < 1 || v > 50 {
				return 0, fmt.Errorf("\"blur\" r must be between 1 and 50, got %q", p.Value)
			}
			haveR = true
		case "s":
			v, err := strconv.Atoi(p.Value)
			if err != nil || v < 1 || v > 50 {
				return 0, fmt.Errorf("\"blur\" s must be between 1 and 50, got %q", p.Value)
			}
			sigma = float64(v)
			haveS = true
		default:
			return 0, fmt.Errorf("unsupported \"blur\" parameter %q", p.Key)
		}
	}

	if !haveR || !haveS {
		return 0, fmt.Errorf("\"blur\" needs both r and s, e.g. blur,r_3,s_2")
	}
	return sigma, nil
}
