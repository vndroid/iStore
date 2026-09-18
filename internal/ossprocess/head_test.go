package ossprocess

import (
	"testing"

	"github.com/vndroid/istore/internal/imagetype"
)

func TestFixedOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want imagetype.Type
		ok   bool
	}{
		{"image/resize,w_10/format,png", imagetype.PNG, true},
		{"image/format,jpeg/format,webp", imagetype.WEBP, true},
		{"image/resize,w_10", imagetype.Unknown, false},
		{"image/format,auto", imagetype.Unknown, false},
		{"image/format,gif", imagetype.Unknown, false},
	} {
		chain, err := Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := chain.FixedOutputFormat()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: format = %v, %v; want %v, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
