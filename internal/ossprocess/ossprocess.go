// Package ossprocess parses the Alibaba Cloud OSS `x-oss-process` query
// parameter into something iStore's pipeline can consume.
//
// The grammar is a slash-separated chain of actions, each with comma-separated
// parameters:
//
//	image/resize,m_fill,w_100,h_100/format,webp/quality,q_80
//	image/info
//
// The first segment names the service; only "image" exists here. Parameters are
// mostly `key_value` pairs, but a few actions (format) take a bare value.
//
// iStore implements `format` and `info` today. Everything else parses into a
// generic Action and is rejected by Chain.Validate with a clear message, rather
// than being silently ignored — an unrecognised transform that returns the
// original image is worse than an error, because the caller cannot tell.
//
// The engine underneath (ported from imgproxy) already supports resize, crop,
// rotate, watermark and the rest. Adding them here is a matter of translating
// parameters into options keys; see Chain.Apply.
package ossprocess

import (
	"fmt"
	"strings"

	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/vips"
)

// QueryKey is the query-string parameter carrying the process chain.
const QueryKey = "x-oss-process"

// Action is one link of the chain: a name plus its raw parameters.
type Action struct {
	Name string
	// Params keeps the parameters in source order. A `key_value` parameter is
	// split; a bare parameter such as the `avif` in `format,avif` is stored
	// with an empty Key.
	Params []Param
}

// Param is a single parameter of an action.
type Param struct {
	Key   string // "" for a bare value
	Value string
}

// Chain is a parsed x-oss-process value.
type Chain struct {
	Actions []Action
}

// Parse reads a raw x-oss-process value. An empty string yields an empty Chain,
// which means "serve the source untouched".
func Parse(raw string) (*Chain, error) {
	if raw == "" {
		return &Chain{}, nil
	}

	segments := strings.Split(raw, "/")
	if segments[0] != "image" {
		return nil, fmt.Errorf("unsupported process service %q, only \"image\" is supported", segments[0])
	}

	c := &Chain{}
	for _, seg := range segments[1:] {
		if seg == "" {
			continue
		}

		parts := strings.Split(seg, ",")
		a := Action{Name: parts[0]}
		if a.Name == "" {
			return nil, fmt.Errorf("empty action in %q", raw)
		}

		for _, p := range parts[1:] {
			if p == "" {
				continue
			}
			// Split on the FIRST underscore only: values may contain more,
			// e.g. `color_FF0000` or a base64 payload.
			if k, v, ok := strings.Cut(p, "_"); ok {
				a.Params = append(a.Params, Param{Key: k, Value: v})
			} else {
				a.Params = append(a.Params, Param{Value: p})
			}
		}

		c.Actions = append(c.Actions, a)
	}

	return c, nil
}

// IsInfo reports whether the chain is a metadata query rather than a transform.
//
// OSS treats `info` as terminal: it describes the source object and cannot be
// combined with transforms. iStore keeps that rule, so a chain is either an
// info query or a transform chain, never both.
func (c *Chain) IsInfo() bool {
	return len(c.Actions) == 1 && c.Actions[0].Name == "info"
}

// Validate rejects chains that are wrong on their face: unknown actions, bad
// parameter shapes, unknown format names.
//
// It deliberately does NOT ask libvips whether it can encode the requested
// format. vips.SupportsSave() calls into the C library, which aborts the process
// with SIGABRT if vips.Init() has not run — so a syntax check that touched it
// could not be called before startup, or from a test. Encoder availability is a
// separate question, answered by CheckEncoders.
func (c *Chain) Validate() error {
	for _, a := range c.Actions {
		switch a.Name {
		case "info":
			if len(c.Actions) != 1 {
				return fmt.Errorf("\"info\" cannot be combined with other actions")
			}
			if len(a.Params) != 0 {
				return fmt.Errorf("\"info\" takes no parameters")
			}
		case "format":
			if _, err := a.format(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported action %q", a.Name)
		}
	}
	return nil
}

// ossFormatNames maps the OSS spelling of a format to iStore's type.
//
// OSS writes JPEG as "jpg"; imagetype's own String() is "jpeg". Both spellings
// are accepted on input, but Info reports the OSS one.
var ossFormatNames = map[string]imagetype.Type{
	"jpg":  imagetype.JPEG,
	"jpeg": imagetype.JPEG,
	"png":  imagetype.PNG,
	"webp": imagetype.WEBP,
	"gif":  imagetype.GIF,
	"avif": imagetype.AVIF,
	"heic": imagetype.HEIC,
	"jxl":  imagetype.JXL,
	"tiff": imagetype.TIFF,
	"bmp":  imagetype.BMP,
}

// OSSFormatName is the OSS spelling for t, used in info responses.
func OSSFormatName(t imagetype.Type) string {
	if t == imagetype.JPEG {
		return "jpg"
	}
	return t.String()
}

// format resolves the target type of a `format` action.
func (a Action) format() (imagetype.Type, error) {
	if len(a.Params) != 1 {
		return imagetype.Unknown, fmt.Errorf("\"format\" takes exactly one value, e.g. format,avif")
	}
	// OSS writes the value bare (`format,avif`). Tolerate `format,f_avif` too,
	// since it costs nothing and reads naturally to anyone used to `w_`/`h_`.
	p := a.Params[0]
	if p.Key != "" && p.Key != "f" {
		return imagetype.Unknown, fmt.Errorf("\"format\" does not take a %q parameter", p.Key)
	}

	name := strings.ToLower(p.Value)
	t, ok := ossFormatNames[name]
	if !ok {
		return imagetype.Unknown, fmt.Errorf("unsupported format %q", name)
	}
	return t, nil
}

// CheckEncoders reports whether this build of libvips can produce every format
// the chain asks for. It must be called after vips.Init.
//
// Kept apart from Validate so the grammar can be checked without a live libvips;
// the HTTP layer calls both, in order.
func (c *Chain) CheckEncoders() error {
	for _, a := range c.Actions {
		if a.Name != "format" {
			continue
		}
		t, err := a.format()
		if err != nil {
			return err
		}
		if !vips.SupportsSave(t) {
			return fmt.Errorf("format %q cannot be produced by this build of libvips", strings.ToLower(a.Params[0].Value))
		}
	}
	return nil
}

// Apply translates the chain into pipeline options.
//
// Only transform chains reach here; call IsInfo first.
func (c *Chain) Apply(o *options.Options) error {
	for _, a := range c.Actions {
		switch a.Name {
		case "format":
			t, err := a.format()
			if err != nil {
				return err
			}
			o.Set(keys.Format, t)
		default:
			return fmt.Errorf("unsupported action %q", a.Name)
		}
	}
	return nil
}

// IsEmpty reports whether the chain asks for nothing.
func (c *Chain) IsEmpty() bool { return len(c.Actions) == 0 }
