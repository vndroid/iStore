package ossprocess

import (
	"math"
	"testing"

	"github.com/vndroid/istore/internal/options/keys"
)

func TestCircleAndRoundedCorners(t *testing.T) {
	if got := applyChain(t, "image/circle,r_100").GetInt(keys.CircleRadius, -1); got != 100 {
		t.Errorf("circle radius = %d, want 100", got)
	}
	if got := applyChain(t, "image/rounded-corners,r_30").GetInt(keys.RoundedCornersRadius, -1); got != 30 {
		t.Errorf("rounded-corners radius = %d, want 30", got)
	}

	// Each sets only its own key, so the pipeline can tell which one was asked
	// for — they differ in whether the image is squared first.
	if applyChain(t, "image/circle,r_10").Has(keys.RoundedCornersRadius) {
		t.Error("circle must not set the rounded-corners radius")
	}
	if applyChain(t, "image/rounded-corners,r_10").Has(keys.CircleRadius) {
		t.Error("rounded-corners must not set the circle radius")
	}
}

func TestCircleOversizedRadiusIsAccepted(t *testing.T) {
	// The source is 400x300, so r_4096 is far past the inscribed circle. OSS
	// clamps instead of failing, and the clamp happens in the pipeline where the
	// post-resize size is known — so the parser must let it through.
	o := applyChain(t, "image/resize,w_100/circle,r_4096")
	if got := o.GetInt(keys.CircleRadius, -1); got != 4096 {
		t.Errorf("radius = %d, want it passed through unclamped", got)
	}
}

func TestShapeRejects(t *testing.T) {
	for _, raw := range []string{
		"image/circle",
		"image/circle,100",    // bare value, OSS spells it r_
		"image/circle,r_0",    // below the range
		"image/circle,r_4097", // above it
		"image/circle,r_abc",
		"image/circle,d_100",
		"image/circle,r_10,r_20",
		"image/rounded-corners",
		"image/rounded-corners,r_0",
		"image/rounded-corners,30",
		// One alpha mask is applied, not two, so the combination is refused
		// rather than silently resolved in circle's favour.
		"image/circle,r_10/rounded-corners,r_10",
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}

func TestShapeNeedsNoSourceSize(t *testing.T) {
	// The radius is clamped against the image the pipeline actually holds, not
	// against the source header, so neither action forces a header read.
	for _, raw := range []string{"image/circle,r_50", "image/rounded-corners,r_50"} {
		c, _ := Parse(raw)
		if c.NeedsSourceSize() {
			t.Errorf("%s must not require the source size", raw)
		}
	}
}

func TestBright(t *testing.T) {
	// The offset is on the 0..255 scale: the ends of OSS's range are full white
	// and full black.
	for raw, want := range map[string]float64{
		"image/bright,0":    0,
		"image/bright,100":  255,
		"image/bright,-100": -255,
		"image/bright,20":   51,
	} {
		got := applyChain(t, raw).GetFloat(keys.Brightness, math.NaN())
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s -> %v, want %v", raw, got, want)
		}
	}
}

func TestContrast(t *testing.T) {
	// 0 must be exactly neutral, or `contrast,0` would quietly alter the image.
	if got := applyChain(t, "image/contrast,0").GetFloat(keys.Contrast, math.NaN()); math.Abs(got-1) > 1e-9 {
		t.Errorf("contrast,0 -> %v, want exactly 1", got)
	}
	// -100 collapses everything to mid-grey.
	if got := applyChain(t, "image/contrast,-100").GetFloat(keys.Contrast, math.NaN()); got != 0 {
		t.Errorf("contrast,-100 -> %v, want 0", got)
	}
	// The curve is monotonic and steep at the top; a linear map would leave the
	// positive half nearly invisible.
	low := applyChain(t, "image/contrast,20").GetFloat(keys.Contrast, 0)
	high := applyChain(t, "image/contrast,100").GetFloat(keys.Contrast, 0)
	if !(low > 1 && high > low) {
		t.Errorf("expected 1 < f(20)=%v < f(100)=%v", low, high)
	}
}

func TestBrightContrastCombine(t *testing.T) {
	// Two separate actions, two separate keys — the pipeline folds them into one
	// linear pass.
	o := applyChain(t, "image/bright,20/contrast,20")
	if !o.Has(keys.Brightness) || !o.Has(keys.Contrast) {
		t.Fatal("both keys should be set")
	}
}

func TestBrightContrastRejects(t *testing.T) {
	for _, raw := range []string{
		"image/bright",
		"image/bright,101",
		"image/bright,-101",
		"image/bright,abc",
		"image/bright,b_20", // bare value only
		"image/bright,10,20",
		"image/contrast",
		"image/contrast,101",
		"image/contrast,-101",
		"image/contrast,abc",
		"image/contrast,c_20",
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}
