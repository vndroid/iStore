package ossprocess

import (
	"fmt"
	"strconv"

	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/processing"
	"github.com/kane/istore/internal/vips/color"
)

// parseSharpen reads `image/sharpen,<50..399>`.
//
// OSS states the range as 50–399 with 100 as the recommended value, without
// saying what the number means. The pipeline's Sharpen is a gaussian sigma,
// where useful values run from roughly 0.5 to 4. Mapping v/100 puts OSS's
// recommended 100 at sigma 1.0 — a normal, visible sharpen — and keeps the ends
// of OSS's range (0.5 and 3.99) inside the sane band.
//
// This is a calibration, not a specification: an image sharpened here will not
// be bit-identical to the same URL on OSS.
func parseSharpen(a Action) (float64, error) {
	if len(a.Params) != 1 {
		return 0, fmt.Errorf("\"sharpen\" takes exactly one value, e.g. sharpen,100")
	}
	p := a.Params[0]
	if p.Key != "" {
		return 0, fmt.Errorf("\"sharpen\" takes a bare value, got %q", p.Key)
	}
	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("\"sharpen\" must be a number, got %q", p.Value)
	}
	if v < 50 || v > 399 {
		return 0, fmt.Errorf("\"sharpen\" must be between 50 and 399, got %d", v)
	}
	return float64(v) / 100, nil
}

// parsePixelate reads `image/pixelate,<block size in pixels>`.
//
// This is an iStore addition, not an OSS action: OSS's effect list has blur,
// sharpen, bright and contrast but no pixelate. The pipeline has it, and it is
// the one effect that reliably makes a face or a licence plate unreadable
// without leaving a recoverable original — a blur strong enough to do that
// usually destroys the rest of the frame too.
//
// The grammar follows the house style for single-value actions (`sharpen`,
// `rotate`): a bare number. The value is the block edge in source pixels, so
// `pixelate,8` averages every 8x8 block.
func parsePixelate(a Action) (int, error) {
	if len(a.Params) != 1 {
		return 0, fmt.Errorf("\"pixelate\" takes exactly one value, e.g. pixelate,8")
	}
	p := a.Params[0]
	if p.Key != "" {
		return 0, fmt.Errorf("\"pixelate\" takes a bare value, got %q", p.Key)
	}
	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("\"pixelate\" must be a number, got %q", p.Value)
	}
	// 1 is a no-op rather than an error, so a caller computing the block size
	// from a zoom level does not have to special-case the smallest one. The
	// upper bound keeps a single block from swallowing any realistic image.
	if v < 1 || v > 1000 {
		return 0, fmt.Errorf("\"pixelate\" must be between 1 and 1000, got %d", v)
	}
	return v, nil
}

// IndexCrop is a parsed `image/indexcrop` action: cut the image into equal
// slices along one axis and keep one of them.
//
// OSS spells it `x_<size>,i_<index>` (vertical cuts every `size` pixels, keep
// the i-th) or `y_<size>,i_<index>`. Despite the name, `x_`/`y_` are the slice
// *width*/*height* in pixels, not a count — a 400px-wide image with `x_100`
// yields four slices, indexed 0..3.
type IndexCrop struct {
	Horizontal bool // true: slice along x (vertical cuts); false: along y
	Size       int
	Index      int
}

func parseIndexCrop(a Action) (*IndexCrop, error) {
	ic := &IndexCrop{}
	var haveAxis, haveIndex bool

	for _, p := range a.Params {
		switch p.Key {
		case "x", "y":
			if haveAxis {
				return nil, fmt.Errorf("\"indexcrop\" takes x or y, not both")
			}
			v, err := strconv.Atoi(p.Value)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("\"indexcrop\" %s must be a positive number, got %q", p.Key, p.Value)
			}
			ic.Horizontal = p.Key == "x"
			ic.Size = v
			haveAxis = true
		case "i":
			v, err := strconv.Atoi(p.Value)
			if err != nil || v < 0 {
				return nil, fmt.Errorf("\"indexcrop\" i must be zero or a positive number, got %q", p.Value)
			}
			ic.Index = v
			haveIndex = true
		default:
			return nil, fmt.Errorf("unsupported \"indexcrop\" parameter %q", p.Key)
		}
	}

	if !haveAxis {
		return nil, fmt.Errorf("\"indexcrop\" needs x or y")
	}
	if !haveIndex {
		return nil, fmt.Errorf("\"indexcrop\" needs i")
	}

	return ic, nil
}

// apply turns the slice into an ordinary crop, which needs the source size to
// know how many slices there are and to reject an index past the last one.
func (ic *IndexCrop) apply(o *options.Options, srcW, srcH int) error {
	span := srcH
	if ic.Horizontal {
		span = srcW
	}
	if span <= 0 {
		return argErrorf("\"indexcrop\" needs the source dimensions")
	}

	// OSS keeps a final short slice rather than discarding it, so a 400px image
	// with x_150 has three slices: 0..149, 150..299, 300..399.
	count := (span + ic.Size - 1) / ic.Size
	if ic.Index >= count {
		return argErrorf("\"indexcrop\" i is %d but the image only has %d slice(s) of %d px",
			ic.Index, count, ic.Size)
	}

	offset := ic.Index * ic.Size
	size := ic.Size
	if offset+size > span {
		size = span - offset
	}

	o.Set(keys.CropGravityType, processing.GravityNorthWest)
	if ic.Horizontal {
		o.Set(keys.CropWidth, float64(size))
		o.Set(keys.CropHeight, float64(srcH))
		o.Set(keys.CropGravityXOffset, float64(offset))
		o.Set(keys.CropGravityYOffset, float64(0))
	} else {
		o.Set(keys.CropWidth, float64(srcW))
		o.Set(keys.CropHeight, float64(size))
		o.Set(keys.CropGravityXOffset, float64(0))
		o.Set(keys.CropGravityYOffset, float64(offset))
	}

	return nil
}

// Trim is a parsed `image/trim` action: remove a uniform border.
//
// This is an iStore addition, not an OSS action. It earns its place because it
// is the one operation that fixes a class of source file rather than restyling
// it — screenshots and exported logos routinely carry a band of background that
// no amount of resizing removes, and trimming at serve time avoids re-cutting
// the originals.
//
//	trim              detect the border colour, threshold 10
//	trim,t_20         same, less tolerant of noise in the border
//	trim,c_FFFFFF     trim white specifically, rather than what the corners show
//	trim,eh_1,ev_1    remove the same amount from opposite sides, keeping the
//	                  subject centred
type Trim struct {
	Threshold float64
	Color     *color.RGB // nil: detect from the image's own border
	EqualHor  bool
	EqualVer  bool
}

// TrimDefaultThreshold matches the pipeline's own default.
const TrimDefaultThreshold = 10.0

func parseTrim(a Action) (*Trim, error) {
	t := &Trim{Threshold: TrimDefaultThreshold}

	seen := map[string]bool{}
	for _, p := range a.Params {
		if p.Key == "" {
			return nil, fmt.Errorf("\"trim\" parameters need a key, got a bare %q", p.Value)
		}
		if seen[p.Key] {
			return nil, fmt.Errorf("\"trim\" parameter %q given twice", p.Key)
		}
		seen[p.Key] = true

		switch p.Key {
		case "t":
			v, err := strconv.Atoi(p.Value)
			if err != nil {
				return nil, fmt.Errorf("\"trim\" t must be a number, got %q", p.Value)
			}
			// 0 means "only exactly equal pixels", which is a legitimate ask for
			// a flat-colour export; 255 trims everything and is refused because
			// it can only produce an empty image.
			if v < 0 || v > 254 {
				return nil, fmt.Errorf("\"trim\" t must be between 0 and 254, got %d", v)
			}
			t.Threshold = float64(v)
		case "c":
			c, err := parseHexColor(p.Value)
			if err != nil {
				return nil, fmt.Errorf("\"trim\" c: %w", err)
			}
			t.Color = &c
		case "eh":
			b, err := trimFlag(p)
			if err != nil {
				return nil, err
			}
			t.EqualHor = b
		case "ev":
			b, err := trimFlag(p)
			if err != nil {
				return nil, err
			}
			t.EqualVer = b
		default:
			return nil, fmt.Errorf("unsupported \"trim\" parameter %q", p.Key)
		}
	}

	return t, nil
}

func trimFlag(p Param) (bool, error) {
	switch p.Value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("\"trim\" %s must be 0 or 1, got %q", p.Key, p.Value)
}

func (t *Trim) apply(o *options.Options) {
	// Setting the threshold is what enables trimming at all
	// (ProcessingOptions.TrimEnabled is Has(TrimThreshold)), so it is written
	// unconditionally even when the caller accepted the default.
	o.Set(keys.TrimThreshold, t.Threshold)

	// Likewise, the presence of the colour key is what switches the pipeline
	// from "detect the border colour" to "trim this colour"
	// (TrimSmart is !Has(TrimColor)), so it is only written when asked for.
	if t.Color != nil {
		o.Set(keys.TrimColor, *t.Color)
	}

	o.Set(keys.TrimEqualHor, t.EqualHor)
	o.Set(keys.TrimEqualVer, t.EqualVer)
}
