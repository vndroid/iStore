package httpserver

import (
	"os"
	"testing"

	"github.com/vndroid/istore/internal/vips"
)

// ServeHTTP calls Chain.CheckEncoders, which asks libvips what it can write, so
// any test that goes in through the handler's front door needs a live libvips —
// without one, vips_type_find trips an assertion and aborts the whole test
// binary rather than failing a single test.
//
// internal/processing brings one up for the same reason.
func TestMain(m *testing.M) {
	c := vips.NewDefaultConfig()
	if err := vips.Init(&c); err != nil {
		panic(err)
	}
	code := m.Run()
	vips.Shutdown()
	os.Exit(code)
}
