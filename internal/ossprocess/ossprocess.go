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
// iStore implements `resize`, `crop`, `indexcrop`, `trim`, `rotate`,
// `auto-orient`, `blur`, `sharpen`, `pixelate`, `bright`, `contrast`, `circle`,
// `rounded-corners`, `watermark`, `quality`, `format` and `info`. Everything
// else
// parses into a
// generic Action and is rejected by Chain.Validate with a clear message, rather
// than being silently ignored — an unrecognised transform that returns the
// original image is worse than an error, because the caller cannot tell.
//
// The engine underneath (ported from imgproxy) also supports padding, extend
// and focus-point gravity. Adding them here is a matter of translating
// parameters into options keys; see Chain.Apply.
package ossprocess

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kane/istore/internal/imagetype"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/options/keys"
	"github.com/kane/istore/internal/vips"
)

// QueryKey is the query-string parameter carrying the process chain.
const QueryKey = "x-oss-process"

// ArgumentError marks a failure caused by the request's own arguments rather
// than by the image or the server.
//
// Most argument checking happens in Validate, before any work starts, and the
// HTTP layer answers those with 400 directly. A few checks can only run once the
// source dimensions are known — indexcrop's slice index is the current one — and
// those surface from Apply, deep inside processing. Without a marker they would
// be reported as "the image could not be processed", which tells the caller
// nothing about the mistake they actually made.
type ArgumentError struct{ Err error }

func (e ArgumentError) Error() string { return e.Err.Error() }
func (e ArgumentError) Unwrap() error { return e.Err }

func argErrorf(format string, args ...any) error {
	return ArgumentError{fmt.Errorf(format, args...)}
}

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
		case "resize":
			if _, err := parseResize(a); err != nil {
				return err
			}
		case "quality":
			if _, err := a.quality(); err != nil {
				return err
			}
		case "crop":
			if _, err := parseCrop(a); err != nil {
				return err
			}
		case "rotate":
			if _, err := parseRotate(a); err != nil {
				return err
			}
		case "auto-orient":
			if _, err := parseAutoOrient(a); err != nil {
				return err
			}
		case "blur":
			if _, err := parseBlur(a); err != nil {
				return err
			}
		case "sharpen":
			if _, err := parseSharpen(a); err != nil {
				return err
			}
		case "pixelate":
			if _, err := parsePixelate(a); err != nil {
				return err
			}
		case "trim":
			if _, err := parseTrim(a); err != nil {
				return err
			}
		case "bright":
			if _, err := parseBright(a); err != nil {
				return err
			}
		case "contrast":
			if _, err := parseContrast(a); err != nil {
				return err
			}
		case "circle", "rounded-corners":
			if _, err := parseRadius(a); err != nil {
				return err
			}
		case "indexcrop":
			if _, err := parseIndexCrop(a); err != nil {
				return err
			}
		case "watermark":
			if _, err := parseWatermark(a); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported action %q", a.Name)
		}
	}

	// Both write an alpha mask over the finished image, and the pipeline applies
	// one mask, not two. Rather than silently letting circle win, say so.
	if c.has("circle") && c.has("rounded-corners") {
		return fmt.Errorf("\"circle\" and \"rounded-corners\" cannot be combined")
	}

	return nil
}

// has reports whether the chain contains an action by that name.
func (c *Chain) has(name string) bool {
	for _, a := range c.Actions {
		if a.Name == name {
			return true
		}
	}
	return false
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

// FormatAuto is the sentinel `format,auto` leaves in the chain: pick the best
// format the client said it accepts.
//
// OSS has no `auto`; this is an iStore addition. It exists because the
// alternative is what the caller would otherwise write by hand — a <picture>
// element with an AVIF <source> and a JPEG fallback — duplicated at every call
// site and stale the moment browser support moves.
const FormatAuto = "auto"

// IsAutoFormat reports whether the chain asks for content-negotiated output.
func (c *Chain) IsAutoFormat() bool {
	for _, a := range c.Actions {
		if a.Name == "format" && len(a.Params) == 1 &&
			strings.ToLower(a.Params[0].Value) == FormatAuto {
			return true
		}
	}
	return false
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
	if name == FormatAuto {
		// Resolved from the request's Accept header, not here.
		return imagetype.Unknown, nil
	}
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
		if t == imagetype.Unknown {
			continue // format,auto: the server picks a format libvips can save
		}
		if !vips.SupportsSave(t) {
			return fmt.Errorf("format %q cannot be produced by this build of libvips", strings.ToLower(a.Params[0].Value))
		}
	}
	return nil
}

// NeedsSourceSize reports whether Apply requires the source dimensions.
//
// resize and indexcrop do; the caller pays a header read for them, which is
// worth avoiding on a plain `format,avif`, the common case.
func (c *Chain) NeedsSourceSize() bool {
	for _, a := range c.Actions {
		if a.Name == "resize" || a.Name == "indexcrop" {
			return true
		}
	}
	return false
}

// Apply translates the chain into pipeline options.
//
// srcW and srcH are the source dimensions; they may be zero when
// NeedsSourceSize reports false. Only transform chains reach here; call IsInfo
// first.
func (c *Chain) Apply(o *options.Options, srcW, srcH int) error {
	for _, a := range c.Actions {
		switch a.Name {
		case "format":
			t, err := a.format()
			if err != nil {
				return err
			}
			// format,auto leaves Format unset; the server sets the Prefer* keys
			// from the Accept header instead, and the pipeline picks from those.
			if t != imagetype.Unknown {
				o.Set(keys.Format, t)
			}

		case "resize":
			r, err := parseResize(a)
			if err != nil {
				return err
			}
			r.Resolve(srcW, srcH).apply(o)

		case "quality":
			q, err := a.quality()
			if err != nil {
				return err
			}
			o.Set(keys.Quality, q)

		case "crop":
			cr, err := parseCrop(a)
			if err != nil {
				return err
			}
			cr.apply(o)

		case "rotate":
			deg, err := parseRotate(a)
			if err != nil {
				return err
			}
			o.Set(keys.Rotate, deg)

		case "auto-orient":
			on, err := parseAutoOrient(a)
			if err != nil {
				return err
			}
			o.Set(keys.AutoRotate, on)

		case "blur":
			sigma, err := parseBlur(a)
			if err != nil {
				return err
			}
			o.Set(keys.Blur, sigma)

		case "sharpen":
			sigma, err := parseSharpen(a)
			if err != nil {
				return err
			}
			o.Set(keys.Sharpen, sigma)

		case "pixelate":
			px, err := parsePixelate(a)
			if err != nil {
				return err
			}
			o.Set(keys.Pixelate, px)

		case "trim":
			tr, err := parseTrim(a)
			if err != nil {
				return err
			}
			tr.apply(o)

		case "bright":
			offset, err := parseBright(a)
			if err != nil {
				return err
			}
			o.Set(keys.Brightness, offset)

		case "contrast":
			factor, err := parseContrast(a)
			if err != nil {
				return err
			}
			o.Set(keys.Contrast, factor)

		case "circle":
			r, err := parseRadius(a)
			if err != nil {
				return err
			}
			o.Set(keys.CircleRadius, r)

		case "rounded-corners":
			r, err := parseRadius(a)
			if err != nil {
				return err
			}
			o.Set(keys.RoundedCornersRadius, r)

		case "indexcrop":
			ic, err := parseIndexCrop(a)
			if err != nil {
				return err
			}
			if err := ic.apply(o, srcW, srcH); err != nil {
				return err
			}

		case "watermark":
			wm, err := parseWatermark(a)
			if err != nil {
				return err
			}
			wm.apply(o)

		default:
			return fmt.Errorf("unsupported action %q", a.Name)
		}
	}
	return nil
}

// quality reads an `image/quality` action.
//
// OSS distinguishes two spellings:
//
//	Q_n  absolute quality
//	q_n  relative quality — for JPEG, n percent OF THE SOURCE's quality
//
// iStore treats both as absolute. Honouring q_ properly would mean recovering
// the source's own quantisation tables and scaling them, which libvips does not
// expose; rejecting q_ instead would break the spelling most OSS URLs actually
// use. So it is accepted and documented as an approximation: for a source saved
// at a quality near iStore's own default the two agree closely, and for a very
// low-quality source q_ will produce a larger file here than OSS would.
func (a Action) quality() (int, error) {
	if len(a.Params) != 1 {
		return 0, fmt.Errorf("\"quality\" takes exactly one value, e.g. quality,q_80")
	}
	p := a.Params[0]
	if p.Key != "q" && p.Key != "Q" {
		return 0, fmt.Errorf("\"quality\" takes q_ or Q_, got %q", p.Key)
	}
	v, err := strconv.Atoi(p.Value)
	if err != nil {
		return 0, fmt.Errorf("\"quality\" value must be a number, got %q", p.Value)
	}
	if v < 1 || v > 100 {
		return 0, fmt.Errorf("\"quality\" must be between 1 and 100, got %d", v)
	}
	return v, nil
}

// IsEmpty reports whether the chain asks for nothing.
func (c *Chain) IsEmpty() bool { return len(c.Actions) == 0 }
