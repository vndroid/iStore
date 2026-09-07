package ossprocess

import (
	"fmt"
	"strconv"
)

// OSS caps both radii at 4096. The pipeline clamps again at request time — to
// the largest inscribed circle for `circle`, to half the shorter side for
// `rounded-corners` — because the useful maximum depends on the size the image
// has by the time the mask is applied, which the parser cannot know.
const maxCornerRadius = 4096

// parseRadius reads the `r_<n>` parameter shared by `circle` and
// `rounded-corners`.
//
//	image/circle,r_100                          a 201x201 result, everything
//	                                            outside the inscribed circle
//	                                            transparent
//	image/rounded-corners,r_30                  original size, 30px corner arcs
//	image/resize,w_300/circle,r_100/format,png
//
// Both actions cut transparent pixels out of the result, so the output format
// matters: PNG and WebP keep the transparency, and JPEG — which cannot — gets
// the background colour instead, white by default. That is OSS's behaviour too,
// and it is why a `circle/format,auto` request is answered with a format that
// has an alpha channel (see determineOutputFormat in internal/processing).
//
// A radius larger than the image is clamped rather than rejected, matching OSS:
// a caller asking for a round avatar from a source of unknown size should get
// one, not a 400.
func parseRadius(a Action) (int, error) {
	if len(a.Params) != 1 {
		return 0, fmt.Errorf("%q takes exactly one parameter, e.g. %s,r_100", a.Name, a.Name)
	}

	p := a.Params[0]
	if p.Key != "r" {
		return 0, fmt.Errorf("%q takes r_, got %q", a.Name, p.Key)
	}

	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("%q r must be a number, got %q", a.Name, p.Value)
	}
	if v < 1 || v > maxCornerRadius {
		return 0, fmt.Errorf("%q r must be between 1 and %d, got %d", a.Name, maxCornerRadius, v)
	}

	return v, nil
}
