package ossprocess

import (
	"testing"

	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
	"github.com/vndroid/istore/internal/processing"
)

func applyChain(t *testing.T, raw string) *options.Options {
	t.Helper()
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(%q): %v", raw, err)
	}
	o := options.New()
	if err := c.Apply(o, srcW, srcH); err != nil {
		t.Fatalf("Apply(%q): %v", raw, err)
	}
	return o
}

func TestCropDefaults(t *testing.T) {
	o := applyChain(t, "image/crop,w_100,h_80")

	if got := o.GetFloat(keys.CropWidth, 0); got != 100 {
		t.Errorf("CropWidth = %v, want 100", got)
	}
	if got := o.GetFloat(keys.CropHeight, 0); got != 80 {
		t.Errorf("CropHeight = %v, want 80", got)
	}
	// OSS anchors at the top-left by default, which is also what makes x_/y_
	// read as plain offsets from the corner.
	if got := options.Get(o, keys.CropGravityType, processing.GravityUnknown); got != processing.GravityNorthWest {
		t.Errorf("gravity = %v, want north-west", got)
	}
	if got := o.GetFloat(keys.CropGravityXOffset, -1); got != 0 {
		t.Errorf("x offset = %v, want 0", got)
	}
}

func TestCropOffsets(t *testing.T) {
	o := applyChain(t, "image/crop,w_100,h_80,x_30,y_20")
	if got := o.GetFloat(keys.CropGravityXOffset, -1); got != 30 {
		t.Errorf("x = %v, want 30", got)
	}
	if got := o.GetFloat(keys.CropGravityYOffset, -1); got != 20 {
		t.Errorf("y = %v, want 20", got)
	}
}

func TestCropGravities(t *testing.T) {
	want := map[string]processing.GravityType{
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
	for name, g := range want {
		o := applyChain(t, "image/crop,w_50,h_50,g_"+name)
		if got := options.Get(o, keys.CropGravityType, processing.GravityUnknown); got != g {
			t.Errorf("g_%s -> %v, want %v", name, got, g)
		}
	}
}

func TestCropSingleDimension(t *testing.T) {
	// Only w: the pipeline clamps the other side to the image.
	o := applyChain(t, "image/crop,w_100")
	if got := o.GetFloat(keys.CropWidth, 0); got != 100 {
		t.Errorf("CropWidth = %v, want 100", got)
	}
	if got := o.GetFloat(keys.CropHeight, -1); got != -1 {
		t.Errorf("CropHeight should be unset, got %v", got)
	}
}

func TestCropRejects(t *testing.T) {
	for _, raw := range []string{
		"image/crop",             // nothing to crop
		"image/crop,x_10,y_10",   // still no size
		"image/crop,w_0",         // zero would be read as a fraction downstream
		"image/crop,w_100,h_abc", // not a number
		"image/crop,w_100,g_up",  // unknown anchor
		"image/crop,w_100,z_1",   // unknown parameter
		"image/crop,w_100,w_200", // repeated
		"image/crop,100",         // bare value
		"image/crop,w_100,x_-5",  // negative offset
	} {
		c, err := Parse(raw)
		if err != nil {
			continue
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%q): expected an error", raw)
		}
	}
}

func TestRotate(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"image/rotate,0", 0},
		{"image/rotate,90", 90},
		{"image/rotate,180", 180},
		{"image/rotate,270", 270},
		{"image/rotate,360", 0}, // normalised
	} {
		o := applyChain(t, tc.raw)
		if got := o.GetInt(keys.Rotate, -1); got != tc.want {
			t.Errorf("%s -> %d, want %d", tc.raw, got, tc.want)
		}
	}

	for _, raw := range []string{
		"image/rotate",
		"image/rotate,-90",
		"image/rotate,400",
		"image/rotate,abc",
		"image/rotate,d_90",
	} {
		c, err := Parse(raw)
		if err != nil {
			continue
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%q): expected an error", raw)
		}
	}
}

func TestRotateFree(t *testing.T) {
	// An angle that is not a multiple of 90 takes the resampling path, which is
	// a different libvips call and therefore a different key. Getting this wrong
	// would send 45 into Rotate, where (45/90)%4 silently becomes 0 — a rotate
	// that does nothing.
	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{"image/rotate,45", 45},
		{"image/rotate,1", 1},
		{"image/rotate,359", 359},
	} {
		o := applyChain(t, tc.raw)
		if got := o.GetFloat(keys.RotateFree, -1); got != tc.want {
			t.Errorf("%s -> %v, want %v", tc.raw, got, tc.want)
		}
		if o.Has(keys.Rotate) {
			t.Errorf("%s must not set the lossless-rotate key", tc.raw)
		}
	}

	// ...and the reverse: a multiple of 90 must stay on the lossless path, which
	// keeps the frame instead of growing it.
	for _, raw := range []string{"image/rotate,90", "image/rotate,180", "image/rotate,270"} {
		if applyChain(t, raw).Has(keys.RotateFree) {
			t.Errorf("%s must not set the resampling key", raw)
		}
	}

	// 360 normalises to 0, which is neither.
	o := applyChain(t, "image/rotate,360")
	if o.GetFloat(keys.RotateFree, 0) != 0 {
		t.Error("rotate,360 should not request a free rotation")
	}
}

func TestAutoOrient(t *testing.T) {
	if got := applyChain(t, "image/auto-orient,1").GetBool(keys.AutoRotate, false); !got {
		t.Error("auto-orient,1 should enable EXIF orientation")
	}
	if got := applyChain(t, "image/auto-orient,0").GetBool(keys.AutoRotate, true); got {
		t.Error("auto-orient,0 should disable EXIF orientation")
	}

	for _, raw := range []string{"image/auto-orient", "image/auto-orient,2", "image/auto-orient,yes"} {
		c, err := Parse(raw)
		if err != nil {
			continue
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%q): expected an error", raw)
		}
	}
}

func TestBlur(t *testing.T) {
	// libvips takes a sigma and derives its own kernel radius, so s_ is the
	// value that reaches the pipeline and r_ is accepted for URL compatibility.
	o := applyChain(t, "image/blur,r_3,s_2")
	if got := o.GetFloat(keys.Blur, 0); got != 2 {
		t.Errorf("blur sigma = %v, want 2", got)
	}

	for _, raw := range []string{
		"image/blur",
		"image/blur,r_3", // OSS requires both
		"image/blur,s_2", //
		"image/blur,r_0,s_2",
		"image/blur,r_3,s_51",
		"image/blur,r_3,s_2,x_1",
	} {
		c, err := Parse(raw)
		if err != nil {
			continue
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%q): expected an error", raw)
		}
	}
}

// TestChainOrder checks that a full chain sets every option it should, since
// each action writes into the same bag and a typo'd key would be silent.
func TestChainOrder(t *testing.T) {
	o := applyChain(t, "image/crop,w_300,h_200,g_center/resize,w_150/rotate,90/blur,r_3,s_2/quality,Q_70/format,webp")

	checks := []struct {
		name string
		ok   bool
	}{
		{"crop width", o.GetFloat(keys.CropWidth, 0) == 300},
		{"crop height", o.GetFloat(keys.CropHeight, 0) == 200},
		{"crop gravity", options.Get(o, keys.CropGravityType, processing.GravityUnknown) == processing.GravityCenter},
		{"resize width", o.GetInt(keys.Width, 0) == 150},
		{"rotate", o.GetInt(keys.Rotate, 0) == 90},
		{"blur", o.GetFloat(keys.Blur, 0) == 2},
		{"quality", o.GetInt(keys.Quality, 0) == 70},
		{"format set", o.Has(keys.Format)},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s was not applied", c.name)
		}
	}
}
