package ossprocess

import (
	"testing"

	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
)

func TestParse(t *testing.T) {
	tests := []struct {
		raw     string
		actions []string // action names, in order
		wantErr bool
	}{
		{"", nil, false},
		{"image/info", []string{"info"}, false},
		{"image/format,avif", []string{"format"}, false},
		{"image/resize,m_fill,w_100,h_100/format,webp", []string{"resize", "format"}, false},
		{"image//format,avif", []string{"format"}, false}, // empty segment tolerated
		{"video/info", nil, true},
		{"image/,avif", nil, true}, // empty action name
	}

	for _, tt := range tests {
		c, err := Parse(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): expected an error", tt.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.raw, err)
			continue
		}
		if len(c.Actions) != len(tt.actions) {
			t.Errorf("Parse(%q): got %d actions, want %d", tt.raw, len(c.Actions), len(tt.actions))
			continue
		}
		for i, name := range tt.actions {
			if c.Actions[i].Name != name {
				t.Errorf("Parse(%q): action %d is %q, want %q", tt.raw, i, c.Actions[i].Name, name)
			}
		}
	}
}

func TestParseParams(t *testing.T) {
	c, err := Parse("image/resize,m_fill,w_100,color_FF00AA,avif")
	if err != nil {
		t.Fatal(err)
	}
	got := c.Actions[0].Params
	want := []Param{
		{Key: "m", Value: "fill"},
		{Key: "w", Value: "100"},
		// Only the first underscore splits, so a value keeping its own
		// underscores survives intact.
		{Key: "color", Value: "FF00AA"},
		{Key: "", Value: "avif"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d params, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("param %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUnderscoreInValue(t *testing.T) {
	c, err := Parse("image/x,k_a_b_c")
	if err != nil {
		t.Fatal(err)
	}
	p := c.Actions[0].Params[0]
	if p.Key != "k" || p.Value != "a_b_c" {
		t.Errorf("got %+v, want {k a_b_c}", p)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{"image/info", false},
		{"image/format,avif", false},
		{"image/format,jpg", false},
		{"image/format,jpeg", false}, // both spellings accepted
		{"image/format,f_avif", false},
		{"image/info/format,avif", true}, // info is terminal
		{"image/info,x_1", true},         // info takes no params
		{"image/format", true},           // format needs a value
		{"image/format,avif,webp", true}, // exactly one value
		{"image/format,tga", true},       // unknown format
		{"image/format,q_avif", true},    // wrong param key
		{"image/resize,w_100", false},    // implemented now
		{"image/resize,m_bogus,w_1", true},
	}

	for _, tt := range tests {
		c, err := Parse(tt.raw)
		if err != nil {
			if !tt.wantErr {
				t.Errorf("Parse(%q): %v", tt.raw, err)
			}
			continue
		}
		err = c.Validate()
		if (err != nil) != tt.wantErr {
			t.Errorf("Validate(%q): err=%v, wantErr=%v", tt.raw, err, tt.wantErr)
		}
	}
}

func TestIsInfo(t *testing.T) {
	for raw, want := range map[string]bool{
		"image/info":             true,
		"image/format,avif":      false,
		"":                       false,
		"image/info/format,avif": false,
	} {
		c, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got := c.IsInfo(); got != want {
			t.Errorf("IsInfo(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestIsAverageHue(t *testing.T) {
	for raw, want := range map[string]bool{
		"image/average-hue":              true,
		"image/info":                     false,
		"image/format,avif":              false,
		"":                               false,
		"image/average-hue/resize,w_100": false,
	} {
		c, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got := c.IsAverageHue(); got != want {
			t.Errorf("IsAverageHue(%q) = %v, want %v", raw, got, want)
		}
	}

	// The two queries are mutually exclusive: the server dispatches on them in
	// turn, and a chain answering true to both would take whichever branch came
	// first.
	c, _ := Parse("image/average-hue")
	if c.IsInfo() {
		t.Error("average-hue must not read as info")
	}
}

func TestQueriesAreTerminal(t *testing.T) {
	// Both describe the source rather than transforming it, so neither can be
	// one link of a chain — there would be nothing for the rest to act on — and
	// neither takes parameters.
	for _, raw := range []string{
		"image/info/format,png",
		"image/resize,w_10/info",
		"image/info,x_1",
		"image/average-hue/format,png",
		"image/resize,w_10/average-hue",
		"image/average-hue,r_1",
		"image/info/average-hue",
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

func TestApplySetsFormat(t *testing.T) {
	c, err := Parse("image/format,avif")
	if err != nil {
		t.Fatal(err)
	}
	o := options.New()
	if err := c.Apply(o, 0, 0); err != nil {
		t.Fatal(err)
	}
	got := options.Get(o, keys.Format, imagetype.Unknown)
	if got != imagetype.AVIF {
		t.Errorf("format = %v, want avif", got)
	}
}

func TestOSSFormatName(t *testing.T) {
	// OSS reports JPEG as "jpg"; everything else matches imagetype's spelling.
	if got := OSSFormatName(imagetype.JPEG); got != "jpg" {
		t.Errorf("JPEG name = %q, want %q", got, "jpg")
	}
	if got := OSSFormatName(imagetype.PNG); got != "png" {
		t.Errorf("PNG name = %q, want %q", got, "png")
	}
}
