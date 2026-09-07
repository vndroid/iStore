package ossprocess

import (
	"encoding/base64"
	"testing"

	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/processing"
	"github.com/kane/istore/internal/vips/color"
)

func TestSharpen(t *testing.T) {
	// OSS's 50..399 maps to a gaussian sigma of 0.5..3.99, putting its
	// recommended 100 at sigma 1.
	for raw, want := range map[string]float64{
		"image/sharpen,50":  0.5,
		"image/sharpen,100": 1.0,
		"image/sharpen,399": 3.99,
	} {
		o := applyChain(t, raw)
		if got := o.GetFloat(keys.Sharpen, 0); got != want {
			t.Errorf("%s -> %v, want %v", raw, got, want)
		}
	}

	for _, raw := range []string{
		"image/sharpen", "image/sharpen,49", "image/sharpen,400",
		"image/sharpen,abc", "image/sharpen,s_100",
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}

func TestIndexCrop(t *testing.T) {
	// Source is 400x300 (srcW/srcH from resize_test.go).
	tests := []struct {
		raw                        string
		wantW, wantH, wantX, wantY float64
	}{
		// Four 100px-wide columns; index 0 is the leftmost.
		{"image/indexcrop,x_100,i_0", 100, 300, 0, 0},
		{"image/indexcrop,x_100,i_3", 100, 300, 300, 0},
		// Three 150px-wide slices: the last one is only 100px, not discarded.
		{"image/indexcrop,x_150,i_2", 100, 300, 300, 0},
		// Two 150px-tall rows.
		{"image/indexcrop,y_150,i_1", 400, 150, 0, 150},
	}

	for _, tt := range tests {
		o := applyChain(t, tt.raw)
		got := [4]float64{
			o.GetFloat(keys.CropWidth, -1),
			o.GetFloat(keys.CropHeight, -1),
			o.GetFloat(keys.CropGravityXOffset, -1),
			o.GetFloat(keys.CropGravityYOffset, -1),
		}
		want := [4]float64{tt.wantW, tt.wantH, tt.wantX, tt.wantY}
		if got != want {
			t.Errorf("%s: got w=%v h=%v x=%v y=%v, want w=%v h=%v x=%v y=%v",
				tt.raw, got[0], got[1], got[2], got[3], want[0], want[1], want[2], want[3])
		}
		if g := options.Get(o, keys.CropGravityType, processing.GravityUnknown); g != processing.GravityNorthWest {
			t.Errorf("%s: gravity = %v, want north-west", tt.raw, g)
		}
	}
}

func TestIndexCropOutOfRange(t *testing.T) {
	// 400px wide in 100px slices is four slices, so i_4 does not exist. This is
	// caught at Apply, not Validate, because it depends on the source size.
	c, err := Parse("image/indexcrop,x_100,i_4")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate should pass without the source size: %v", err)
	}
	if err := c.Apply(options.New(), srcW, srcH); err == nil {
		t.Error("Apply should reject an index past the last slice")
	}
}

func TestIndexCropNeedsSourceSize(t *testing.T) {
	c, _ := Parse("image/indexcrop,x_100,i_0")
	if !c.NeedsSourceSize() {
		t.Error("indexcrop must report that it needs the source size")
	}
}

func TestIndexCropRejects(t *testing.T) {
	for _, raw := range []string{
		"image/indexcrop",
		"image/indexcrop,x_100",           // no index
		"image/indexcrop,i_0",             // no axis
		"image/indexcrop,x_100,y_100,i_0", // both axes
		"image/indexcrop,x_0,i_0",
		"image/indexcrop,x_100,i_-1",
		"image/indexcrop,x_100,i_0,z_1",
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestWatermark(t *testing.T) {
	raw := "image/watermark,image_" + b64url("logo.png") + ",t_50,g_nw,x_10,y_20"
	o := applyChain(t, raw)

	if got := o.GetString(KeyWatermarkPath, ""); got != "logo.png" {
		t.Errorf("path = %q, want %q", got, "logo.png")
	}
	if got := o.GetFloat(keys.WatermarkOpacity, -1); got != 0.5 {
		t.Errorf("opacity = %v, want 0.5", got)
	}
	if got := options.Get(o, keys.WatermarkPosition, processing.GravityUnknown); got != processing.GravityNorthWest {
		t.Errorf("position = %v, want north-west", got)
	}
	if got := o.GetFloat(keys.WatermarkXOffset, -1); got != 10 {
		t.Errorf("x = %v, want 10", got)
	}
}

func TestWatermarkDefaults(t *testing.T) {
	o := applyChain(t, "image/watermark,image_"+b64url("logo.png"))
	if got := o.GetFloat(keys.WatermarkOpacity, -1); got != 1 {
		t.Errorf("default opacity = %v, want 1", got)
	}
	// OSS anchors a watermark bottom-right by default.
	if got := options.Get(o, keys.WatermarkPosition, processing.GravityUnknown); got != processing.GravitySouthEast {
		t.Errorf("default position = %v, want south-east", got)
	}
}

func TestWatermarkBase64Variants(t *testing.T) {
	// OSS documents unpadded base64url, but padded and standard-alphabet forms
	// turn up in the wild.
	key := "dir/sub/logo (1).png"
	for name, enc := range map[string]string{
		"rawurl": base64.RawURLEncoding.EncodeToString([]byte(key)),
		"url":    base64.URLEncoding.EncodeToString([]byte(key)),
		"rawstd": base64.RawStdEncoding.EncodeToString([]byte(key)),
		"std":    base64.StdEncoding.EncodeToString([]byte(key)),
	} {
		o := applyChain(t, "image/watermark,image_"+enc)
		if got := o.GetString(KeyWatermarkPath, ""); got != key {
			t.Errorf("%s: path = %q, want %q", name, got, key)
		}
	}
}

func TestWatermarkRejects(t *testing.T) {
	for _, raw := range []string{
		"image/watermark",                         // no image
		"image/watermark,t_50",                    // still no image
		"image/watermark,text_" + b64url("hello"), // text is not supported
		"image/watermark,image_!!!",               // not base64
		"image/watermark,image_" + b64url(""),     // empty key
		"image/watermark,image_" + b64url("a") + ",t_101",
		"image/watermark,image_" + b64url("a") + ",g_up",
		"image/watermark,image_" + b64url("a") + ",z_1",
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}

func TestFormatAuto(t *testing.T) {
	c, err := Parse("image/format,auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("format,auto should validate: %v", err)
	}
	if !c.IsAutoFormat() {
		t.Error("IsAutoFormat should report true")
	}

	// auto leaves Format unset; the server fills in the Prefer* keys instead.
	o := options.New()
	if err := c.Apply(o, srcW, srcH); err != nil {
		t.Fatal(err)
	}
	if o.Has(keys.Format) {
		t.Error("format,auto should not set an explicit format")
	}

	c2, _ := Parse("image/format,avif")
	if c2.IsAutoFormat() {
		t.Error("format,avif is not auto")
	}
}

func TestPixelate(t *testing.T) {
	for raw, want := range map[string]int{
		"image/pixelate,1":    1, // a no-op, deliberately not an error
		"image/pixelate,8":    8,
		"image/pixelate,1000": 1000,
	} {
		o := applyChain(t, raw)
		if got := o.GetInt(keys.Pixelate, 0); got != want {
			t.Errorf("%s -> %d, want %d", raw, got, want)
		}
	}

	for _, raw := range []string{
		"image/pixelate",
		"image/pixelate,0",
		"image/pixelate,-1",
		"image/pixelate,1001",
		"image/pixelate,abc",
		"image/pixelate,p_8",
		"image/pixelate,8,16",
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}

func TestTrimDefaults(t *testing.T) {
	o := applyChain(t, "image/trim")

	// The threshold key is what enables trimming at all, so a bare `trim` must
	// still write it.
	if !o.Has(keys.TrimThreshold) {
		t.Fatal("bare trim must set the threshold, otherwise the pipeline skips it")
	}
	if got := o.GetFloat(keys.TrimThreshold, -1); got != TrimDefaultThreshold {
		t.Errorf("threshold = %v, want %v", got, TrimDefaultThreshold)
	}
	// No colour means "detect from the image's own border".
	if o.Has(keys.TrimColor) {
		t.Error("bare trim must not set a colour, or the pipeline stops auto-detecting")
	}
	if o.GetBool(keys.TrimEqualHor, true) || o.GetBool(keys.TrimEqualVer, true) {
		t.Error("equal-sides flags should default to off")
	}
}

func TestTrimParams(t *testing.T) {
	o := applyChain(t, "image/trim,t_20,c_FFFFFF,eh_1,ev_1")

	if got := o.GetFloat(keys.TrimThreshold, -1); got != 20 {
		t.Errorf("threshold = %v, want 20", got)
	}
	if !o.Has(keys.TrimColor) {
		t.Fatal("c_ should set an explicit trim colour")
	}
	c := options.Get(o, keys.TrimColor, color.Black)
	if c.R != 0xFF || c.G != 0xFF || c.B != 0xFF {
		t.Errorf("colour = %v, want white", c)
	}
	if !o.GetBool(keys.TrimEqualHor, false) || !o.GetBool(keys.TrimEqualVer, false) {
		t.Error("eh_1/ev_1 should enable the equal-sides flags")
	}
}

func TestTrimThresholdBounds(t *testing.T) {
	// 0 is meaningful: trim only pixels exactly equal to the border colour.
	if got := applyChain(t, "image/trim,t_0").GetFloat(keys.TrimThreshold, -1); got != 0 {
		t.Errorf("t_0 -> %v, want 0", got)
	}
	if got := applyChain(t, "image/trim,t_254").GetFloat(keys.TrimThreshold, -1); got != 254 {
		t.Errorf("t_254 -> %v, want 254", got)
	}
}

func TestTrimRejects(t *testing.T) {
	for _, raw := range []string{
		"image/trim,t_-1",
		"image/trim,t_255", // would trim everything
		"image/trim,t_abc",
		"image/trim,c_XYZ",
		"image/trim,c_FFF", // must be six hex digits
		"image/trim,eh_2",
		"image/trim,z_1",
		"image/trim,10",      // bare value
		"image/trim,t_1,t_2", // repeated
	} {
		if c, err := Parse(raw); err == nil {
			if err := c.Validate(); err == nil {
				t.Errorf("Validate(%q): expected an error", raw)
			}
		}
	}
}

func TestTrimNeedsNoSourceSize(t *testing.T) {
	c, _ := Parse("image/trim,t_20")
	if c.NeedsSourceSize() {
		t.Error("trim works from the pixels, not the declared size, so it must not force a header read")
	}
}
