package ossprocess

import (
	"testing"

	"github.com/kane/istore/internal/processing"
)

// resolve is a helper: parse one resize action and resolve it against a source.
func resolve(t *testing.T, raw string, srcW, srcH int) Target {
	t.Helper()
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(%q): %v", raw, err)
	}
	r, err := parseResize(c.Actions[0])
	if err != nil {
		t.Fatalf("parseResize(%q): %v", raw, err)
	}
	return r.Resolve(srcW, srcH)
}

// The source is 400x300 (4:3) throughout, so the arithmetic is easy to check by
// hand and asymmetric enough to catch a swapped axis.
const (
	srcW = 400
	srcH = 300
)

func TestResizeModes(t *testing.T) {
	tests := []struct {
		raw      string
		wantW    int
		wantH    int
		wantType processing.ResizeType
		wantPad  bool
		wantNoop bool
		why      string
	}{
		{
			raw: "image/resize,m_lfit,w_200,h_200", wantW: 200, wantH: 200,
			wantType: processing.ResizeFit,
			why:      "lfit fits inside the box; the pipeline scales by the larger shrink, giving 200x150",
		},
		{
			raw: "image/resize,m_fill,w_200,h_200", wantW: 200, wantH: 200,
			wantType: processing.ResizeFill,
			why:      "fill covers the box then crops to exactly 200x200",
		},
		{
			raw: "image/resize,m_fixed,w_200,h_200", wantW: 200, wantH: 200,
			wantType: processing.ResizeForce,
			why:      "fixed ignores aspect ratio",
		},
		{
			raw: "image/resize,m_pad,w_200,h_200", wantW: 200, wantH: 200,
			wantType: processing.ResizeFit, wantPad: true,
			why: "pad fits inside then extends to the full box",
		},
		{
			// Covering a 200x200 box from 400x300 needs scale = max(200/400, 200/300)
			// = 0.667, so the result is 267x200 — wider than the box, uncropped.
			raw: "image/resize,m_mfit,w_200,h_200", wantW: 267, wantH: 200,
			wantType: processing.ResizeForce,
			why:      "mfit covers the box without cropping, so one side overflows",
		},
	}

	for _, tt := range tests {
		got := resolve(t, tt.raw, srcW, srcH)
		if got.Noop != tt.wantNoop {
			t.Errorf("%s: Noop = %v, want %v (%s)", tt.raw, got.Noop, tt.wantNoop, tt.why)
			continue
		}
		if got.W != tt.wantW || got.H != tt.wantH {
			t.Errorf("%s: got %dx%d, want %dx%d (%s)", tt.raw, got.W, got.H, tt.wantW, tt.wantH, tt.why)
		}
		if got.Type != tt.wantType {
			t.Errorf("%s: type = %v, want %v", tt.raw, got.Type, tt.wantType)
		}
		if got.Pad != tt.wantPad {
			t.Errorf("%s: pad = %v, want %v", tt.raw, got.Pad, tt.wantPad)
		}
	}
}

func TestResizeSingleSide(t *testing.T) {
	// Giving one side scales proportionally regardless of mode.
	for _, raw := range []string{
		"image/resize,w_200",
		"image/resize,m_lfit,w_200",
		"image/resize,m_fill,w_200",
	} {
		got := resolve(t, raw, srcW, srcH)
		if got.W != 200 || got.H != 150 {
			t.Errorf("%s: got %dx%d, want 200x150", raw, got.W, got.H)
		}
	}

	got := resolve(t, "image/resize,h_150", srcW, srcH)
	if got.W != 200 || got.H != 150 {
		t.Errorf("h_150: got %dx%d, want 200x150", got.W, got.H)
	}
}

func TestResizeLongestShortest(t *testing.T) {
	// l_ constrains the longest side: 400x300 with l_200 -> 200x150.
	got := resolve(t, "image/resize,l_200", srcW, srcH)
	if got.W != 200 || got.H != 200 || got.Type != processing.ResizeFit {
		t.Errorf("l_200: got %dx%d type=%v, want a 200x200 fit box", got.W, got.H, got.Type)
	}

	// s_ constrains the shortest side: 400x300 with s_150 -> 200x150.
	got = resolve(t, "image/resize,s_150", srcW, srcH)
	if got.W != 200 || got.H != 150 {
		t.Errorf("s_150: got %dx%d, want 200x150", got.W, got.H)
	}
}

func TestResizePercent(t *testing.T) {
	got := resolve(t, "image/resize,p_50", srcW, srcH)
	if got.W != 200 || got.H != 150 {
		t.Errorf("p_50: got %dx%d, want 200x150", got.W, got.H)
	}

	// p_ above 100 enlarges, which limit_1 forbids by default.
	got = resolve(t, "image/resize,p_200", srcW, srcH)
	if !got.Noop {
		t.Errorf("p_200 with the default limit should be a no-op, got %dx%d", got.W, got.H)
	}

	got = resolve(t, "image/resize,p_200,limit_0", srcW, srcH)
	if got.Noop || got.W != 800 || got.H != 600 {
		t.Errorf("p_200,limit_0: got %dx%d noop=%v, want 800x600", got.W, got.H, got.Noop)
	}
}

// TestResizeLimit pins OSS's default behaviour: asking for something bigger than
// the source returns the source untouched rather than upscaling.
func TestResizeLimit(t *testing.T) {
	tests := []struct {
		raw      string
		wantNoop bool
	}{
		{"image/resize,m_lfit,w_800,h_600", true},          // both sides bigger
		{"image/resize,m_lfit,w_800,h_100", false},         // lfit scales by the smaller ratio, so it shrinks
		{"image/resize,m_fill,w_800,h_600", true},          // fill scales by the larger ratio
		{"image/resize,m_fill,w_800,h_100", true},          // ...so one bigger side is enough
		{"image/resize,m_fixed,w_800,h_600", true},         // fixed enlarges outright
		{"image/resize,m_lfit,w_800,h_600,limit_0", false}, // opting out enlarges
		{"image/resize,m_lfit,w_200,h_200", false},         // plain shrink
	}

	for _, tt := range tests {
		got := resolve(t, tt.raw, srcW, srcH)
		if got.Noop != tt.wantNoop {
			t.Errorf("%s: Noop = %v, want %v", tt.raw, got.Noop, tt.wantNoop)
		}
	}
}

func TestResizeColor(t *testing.T) {
	got := resolve(t, "image/resize,m_pad,w_200,h_200,color_FF8000", srcW, srcH)
	if got.Color.R != 0xFF || got.Color.G != 0x80 || got.Color.B != 0x00 {
		t.Errorf("colour = %v, want [255 128 0]", got.Color)
	}

	// The default pad colour is white.
	got = resolve(t, "image/resize,m_pad,w_200,h_200", srcW, srcH)
	if got.Color.R != 0xFF || got.Color.G != 0xFF || got.Color.B != 0xFF {
		t.Errorf("default colour = %v, want white", got.Color)
	}
}

func TestResizeRejects(t *testing.T) {
	bad := []string{
		"image/resize",                         // nothing to do
		"image/resize,limit_1",                 // still nothing to do
		"image/resize,w_200,l_100",             // box and single-side mixed
		"image/resize,l_100,s_100",             // both single-side
		"image/resize,p_50,w_100",              // percent with a box
		"image/resize,m_crop,w_100",            // unknown mode
		"image/resize,w_0",                     // out of range
		"image/resize,w_abc",                   // not a number
		"image/resize,limit_2",                 // not a flag
		"image/resize,color_XYZ,m_pad,w_1,h_1", // bad colour
		"image/resize,color_FFF,m_pad,w_1,h_1", // wrong colour length
		"image/resize,w_100,w_200",             // repeated parameter
		"image/resize,200",                     // bare value
		"image/resize,q_80",                    // wrong parameter for this action
	}

	for _, raw := range bad {
		c, err := Parse(raw)
		if err != nil {
			continue // rejected at parse time is fine too
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%q): expected an error", raw)
		}
	}
}

func TestQuality(t *testing.T) {
	for _, raw := range []string{"image/quality,q_80", "image/quality,Q_80"} {
		c, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", raw, err)
			continue
		}
		q, err := c.Actions[0].quality()
		if err != nil {
			t.Errorf("quality(%q): %v", raw, err)
			continue
		}
		if q != 80 {
			t.Errorf("quality(%q) = %d, want 80", raw, q)
		}
	}

	for _, raw := range []string{
		"image/quality",
		"image/quality,80",
		"image/quality,q_0",
		"image/quality,q_101",
		"image/quality,q_abc",
		"image/quality,w_80",
		"image/quality,q_80,Q_90",
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

func TestNeedsSourceSize(t *testing.T) {
	for raw, want := range map[string]bool{
		"image/format,avif":              false,
		"image/quality,q_80":             false,
		"image/resize,w_100":             true,
		"image/resize,w_100/format,avif": true,
		"image/format,avif/quality,q_80": false,
	} {
		c, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got := c.NeedsSourceSize(); got != want {
			t.Errorf("NeedsSourceSize(%q) = %v, want %v", raw, got, want)
		}
	}
}
