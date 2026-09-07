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
	// OSS anchors a watermark bottom-right by default, 10px off each edge.
	if got := options.Get(o, keys.WatermarkPosition, processing.GravityUnknown); got != processing.GravitySouthEast {
		t.Errorf("default position = %v, want south-east", got)
	}
	if got := o.GetFloat(keys.WatermarkXOffset, -1); got != 10 {
		t.Errorf("default x = %v, want 10", got)
	}
	if got := o.GetFloat(keys.WatermarkYOffset, -1); got != 10 {
		t.Errorf("default y = %v, want 10", got)
	}
	// Nothing about text should be set for an image-only watermark.
	if o.Has(KeyWatermarkText) {
		t.Error("an image watermark must not carry text options")
	}
}

func TestWatermarkScale(t *testing.T) {
	// P_ is a percentage of the base image, which the pipeline spells as a
	// 0..1 scale.
	o := applyChain(t, "image/watermark,image_"+b64url("logo.png")+",P_20")
	if got := o.GetFloat(keys.WatermarkScale, -1); got != 0.2 {
		t.Errorf("P_20 -> %v, want 0.2", got)
	}
	// Absent, the watermark keeps its own size and the key stays unset, because
	// the pipeline reads any positive value as a request to rescale.
	if applyChain(t, "image/watermark,image_"+b64url("logo.png")).Has(keys.WatermarkScale) {
		t.Error("no P_ should leave the scale unset")
	}
}

func TestWatermarkVOffset(t *testing.T) {
	// voffset is the offset from the centre line, so it is signed and lands in
	// the same key as y_ — which is why giving both is refused.
	o := applyChain(t, "image/watermark,image_"+b64url("l.png")+",g_center,voffset_-30")
	if got := o.GetFloat(keys.WatermarkYOffset, 0); got != -30 {
		t.Errorf("voffset_-30 -> %v, want -30", got)
	}
}

func TestWatermarkFill(t *testing.T) {
	o := applyChain(t, "image/watermark,image_"+b64url("l.png")+",fill_1,padx_20,pady_30")

	// Tiling is a gravity in the pipeline, and the offsets become the gaps
	// between copies rather than a position.
	if got := options.Get(o, keys.WatermarkPosition, processing.GravityUnknown); got != processing.GravityReplicate {
		t.Errorf("fill_1 position = %v, want replicate", got)
	}
	if got := o.GetFloat(keys.WatermarkXOffset, -1); got != 20 {
		t.Errorf("padx -> %v, want 20", got)
	}
	if got := o.GetFloat(keys.WatermarkYOffset, -1); got != 30 {
		t.Errorf("pady -> %v, want 30", got)
	}

	// fill_0 is the default and must not turn into tiling.
	o = applyChain(t, "image/watermark,image_"+b64url("l.png")+",fill_0")
	if got := options.Get(o, keys.WatermarkPosition, processing.GravityUnknown); got == processing.GravityReplicate {
		t.Error("fill_0 should leave the anchor alone")
	}
}

func TestWatermarkText(t *testing.T) {
	raw := "image/watermark,text_" + b64url("© iStore") +
		",type_" + b64url("wqy-zenhei") + ",color_FF0000,size_24,shadow_50,rotate_30,t_80"
	o := applyChain(t, raw)

	if got := o.GetString(KeyWatermarkText, ""); got != "© iStore" {
		t.Errorf("text = %q", got)
	}
	// The OSS font identifier is mapped to a family list fontconfig can resolve,
	// with a generic fallback: the exact face is the host's business.
	if got := o.GetString(KeyWatermarkFont, ""); got != "WenQuanYi Zen Hei,sans" {
		t.Errorf("font = %q", got)
	}
	c := options.Get(o, KeyWatermarkColor, color.Black)
	if c.R != 0xFF || c.G != 0 || c.B != 0 {
		t.Errorf("colour = %v, want red", c)
	}
	if got := o.GetInt(KeyWatermarkSize, 0); got != 24 {
		t.Errorf("size = %d, want 24", got)
	}
	if got := o.GetFloat(KeyWatermarkShadow, -1); got != 0.5 {
		t.Errorf("shadow = %v, want 0.5", got)
	}
	if got := o.GetInt(KeyWatermarkRotate, -1); got != 30 {
		t.Errorf("rotate = %d, want 30", got)
	}
	if got := o.GetFloat(keys.WatermarkOpacity, -1); got != 0.8 {
		t.Errorf("opacity = %v, want 0.8", got)
	}
	// No object key, so nothing should ask the provider to read one.
	if o.Has(KeyWatermarkPath) {
		t.Error("a text watermark must not set an object key")
	}
}

func TestWatermarkTextDefaults(t *testing.T) {
	o := applyChain(t, "image/watermark,text_"+b64url("hi"))

	if got := o.GetInt(KeyWatermarkSize, 0); got != 40 {
		t.Errorf("default size = %d, want 40", got)
	}
	if got := o.GetFloat(KeyWatermarkShadow, -1); got != 0 {
		t.Errorf("default shadow = %v, want 0", got)
	}
	c := options.Get(o, KeyWatermarkColor, color.White)
	if c != color.Black {
		t.Errorf("default colour = %v, want black", c)
	}
}

func TestWatermarkImageAndText(t *testing.T) {
	// order/align/interval only mean something when both layers exist, so they
	// are only written then.
	raw := "image/watermark,image_" + b64url("l.png") + ",text_" + b64url("hi") +
		",order_1,align_1,interval_12"
	o := applyChain(t, raw)

	if got := o.GetInt(KeyWatermarkOrder, -1); got != 1 {
		t.Errorf("order = %d, want 1", got)
	}
	if got := o.GetInt(KeyWatermarkAlign, -1); got != 1 {
		t.Errorf("align = %d, want 1", got)
	}
	if got := o.GetInt(KeyWatermarkInterval, -1); got != 12 {
		t.Errorf("interval = %d, want 12", got)
	}

	// OSS's default alignment is bottom.
	o = applyChain(t, "image/watermark,image_"+b64url("l.png")+",text_"+b64url("hi"))
	if got := o.GetInt(KeyWatermarkAlign, -1); got != 2 {
		t.Errorf("default align = %d, want 2 (bottom)", got)
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
		"image/watermark",                     // neither image nor text
		"image/watermark,t_50",                // still neither
		"image/watermark,image_!!!",           // not base64
		"image/watermark,image_" + b64url(""), // empty key
		"image/watermark,image_" + b64url("a") + ",t_101",
		"image/watermark,image_" + b64url("a") + ",g_up",
		"image/watermark,image_" + b64url("a") + ",z_1",
		// Each of these asks for two things at once, or for a parameter that
		// belongs to the other kind of watermark. Honouring one and dropping the
		// other silently is the failure this package exists to avoid.
		"image/watermark,image_" + b64url("a") + ",y_10,voffset_10",
		"image/watermark,image_" + b64url("a") + ",fill_1,g_nw",
		"image/watermark,image_" + b64url("a") + ",fill_1,x_10",
		"image/watermark,image_" + b64url("a") + ",padx_10",
		"image/watermark,image_" + b64url("a") + ",size_20",
		"image/watermark,image_" + b64url("a") + ",color_FF0000",
		"image/watermark,text_" + b64url("hi") + ",P_50",
		"image/watermark,text_" + b64url("hi") + ",order_1",
		"image/watermark,image_" + b64url("a") + ",order_1",
		"image/watermark,image_" + b64url("a") + ",P_0",
		"image/watermark,image_" + b64url("a") + ",P_101",
		"image/watermark,text_" + b64url("hi") + ",size_0",
		"image/watermark,text_" + b64url("hi") + ",size_1001",
		"image/watermark,text_" + b64url("hi") + ",shadow_101",
		"image/watermark,text_" + b64url("hi") + ",rotate_361",
		"image/watermark,text_" + b64url("hi") + ",color_XYZ",
		"image/watermark,image_" + b64url("a") + ",fill_2",
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
