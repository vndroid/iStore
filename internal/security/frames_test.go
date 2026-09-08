package security

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
)

func framesChecker(t *testing.T, limit int) *Checker {
	t.Helper()

	c := NewDefaultConfig()
	c.MaxAnimationFrames = limit

	checker, err := New(&c)
	if err != nil {
		t.Fatal(err)
	}
	return checker
}

// The default is a deliberate departure from imgproxy's 1, and the reason is not
// visible from the value itself — so it gets a test rather than only a comment.
// imgproxy's default means "never animate", which for a service whose job is to
// answer the way OSS answers is the wrong answer to give silently.
func TestDefaultAllowsAnimation(t *testing.T) {
	c := NewDefaultConfig()

	if c.MaxAnimationFrames <= 1 {
		t.Fatalf("MaxAnimationFrames = %d: at 1 or below, every animation is flattened "+
			"to its first frame and no animated output is possible", c.MaxAnimationFrames)
	}
	if c.MaxAnimationFrames != DefaultMaxAnimationFrames {
		t.Errorf("MaxAnimationFrames = %d, want DefaultMaxAnimationFrames (%d)",
			c.MaxAnimationFrames, DefaultMaxAnimationFrames)
	}
}

// The real budget is CheckDimensions' width x height x frames, not this cap, and
// that has not moved. A test rather than a comment because the whole argument for
// raising the frame cap rests on it.
func TestPixelBudgetStillBindsAnimations(t *testing.T) {
	c := NewDefaultConfig()

	checker, err := New(&c)
	if err != nil {
		t.Fatal(err)
	}

	o := options.New()

	// 300 frames of 1000x1000 is 300 MP against a 250 MP budget.
	if err := checker.CheckDimensions(o, 1000, 1000, 300); err == nil {
		t.Error("300 frames of 1000x1000 passed the resolution check; the pixel budget is not binding")
	}

	// The same frame count at a size that fits must still be allowed, or the
	// budget is doing more than it should.
	if err := checker.CheckDimensions(o, 500, 500, 300); err != nil {
		t.Errorf("300 frames of 500x500 (75 MP) was refused: %v", err)
	}
}

// The budget is a deliberate match to OSS's, and the number is the whole point
// of the match — so it is asserted rather than left to a comment. A still image
// at the ceiling passes; one pixel past it does not.
func TestPixelBudgetMatchesOSS(t *testing.T) {
	c := NewDefaultConfig()

	if c.MaxSrcResolution != DefaultMaxSrcResolution || DefaultMaxSrcResolution != 250_000_000 {
		t.Errorf("MaxSrcResolution = %d, want 250000000 to match OSS", c.MaxSrcResolution)
	}

	checker, err := New(&c)
	if err != nil {
		t.Fatal(err)
	}

	o := options.New()

	// 20000x12500 is exactly 250 MP.
	if err := checker.CheckDimensions(o, 20000, 12500, 1); err != nil {
		t.Errorf("a source at exactly the ceiling was refused: %v", err)
	}
	if err := checker.CheckDimensions(o, 20000, 12501, 1); err == nil {
		t.Error("a source past the ceiling was accepted")
	}

	// The size that moved: 63 MP was refused under the old 50 MP default and is
	// well within OSS's, which is the reason for the change.
	if err := checker.CheckDimensions(o, 9000, 7000, 1); err != nil {
		t.Errorf("a 63 MP source was refused; OSS accepts it: %v", err)
	}
}

func TestCheckAnimationFrames(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		frames int
		wantOK bool
	}{
		{"under the cap", 300, 12, true},
		{"exactly at the cap", 300, 300, true},
		{"one over", 300, 301, false},
		{"far over", 300, 5000, false},
		{"a still image against a cap of 1", 1, 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := framesChecker(t, tt.limit).CheckAnimationFrames(options.New(), tt.frames)

			if tt.wantOK && err != nil {
				t.Errorf("CheckAnimationFrames(%d) with limit %d = %v, want nil", tt.frames, tt.limit, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("CheckAnimationFrames(%d) with limit %d = nil, want an error", tt.frames, tt.limit)
			}
		})
	}
}

// The refusal has to be usable: a status the HTTP layer can map, and a message
// naming both numbers. Its resolution sibling says only "Invalid source image",
// which tells a caller nothing they can act on — and here they can act, by
// asking for a still format instead.
func TestAnimationFramesErrorIsActionable(t *testing.T) {
	err := framesChecker(t, 300).CheckAnimationFrames(options.New(), 512)
	if err == nil {
		t.Fatal("expected an error")
	}

	coded, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error carries no status code: %v", err)
	}
	if coded.StatusCode() != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", coded.StatusCode(), http.StatusUnprocessableEntity)
	}

	pub, ok := err.(interface{ PublicMessage() string })
	if !ok {
		t.Fatalf("error carries no public message: %v", err)
	}
	msg := pub.PublicMessage()
	if !strings.Contains(msg, "512") || !strings.Contains(msg, "300") {
		t.Errorf("public message %q names neither the frame count nor the limit", msg)
	}
}

// Like every other security limit, the cap can be overridden per request through
// the options bag.
func TestCheckAnimationFramesRespectsPerRequestOverride(t *testing.T) {
	checker := framesChecker(t, 300)

	o := options.New()
	o.Set(keys.MaxAnimationFrames, 10)

	if err := checker.CheckAnimationFrames(o, 50); err == nil {
		t.Error("the per-request cap of 10 was ignored")
	}
	if err := checker.CheckAnimationFrames(o, 8); err != nil {
		t.Errorf("8 frames against a per-request cap of 10 was refused: %v", err)
	}
}
