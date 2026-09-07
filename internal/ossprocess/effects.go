package ossprocess

import (
	"fmt"
	"strconv"

	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/processing"
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
