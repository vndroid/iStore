// Package source resolves a request path to the bytes of a source object.
//
// Two backends implement Source. Local reads from a directory on disk; Upstream
// fetches over HTTP from a fixed origin. Which one a deployment gets is decided
// once at startup, by ISTORE_ROOT or ISTORE_UPSTREAM, and nothing above this
// package knows which one it is holding.
//
// The interface is shaped by what the handlers actually need, which is three
// different amounts of the object:
//
//   - Stat, for the cache key, needs no body at all;
//   - OpenHeader, for info and for resize's source dimensions, needs 64 KiB;
//   - Open, for any decode, needs all of it.
//
// Keeping those separate is what makes the info endpoint cheap. Against local
// disk the distinction is nearly free either way, but against an origin it is
// the difference between a 64 KiB ranged response and pulling a 40 MB JPEG
// across the network to read its header.
package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
)

// HeaderWindow is the largest prefix Object.Peek can return.
//
// It matches imageinfo.HeaderBytes, which is the only caller that asks for a
// large one. The package does not import imageinfo — the dependency would point
// the wrong way — so httpserver asserts the two agree at compile time.
const HeaderWindow = 64 << 10

var (
	// ErrNotFound means the object does not exist.
	ErrNotFound = errors.New("source: not found")
	// ErrOutsideRoot means the resolved path escaped the root, or the joined
	// URL escaped the upstream's base path.
	ErrOutsideRoot = errors.New("source: path escapes the root directory")
	// ErrNotRegular means the path exists but is a directory, device, socket...
	ErrNotRegular = errors.New("source: not a regular file")
	// ErrTooLarge means the object outran the limit given to ReadAll.
	ErrTooLarge = errors.New("source: object exceeds the read limit")
)

// Source is where source objects come from.
type Source interface {
	// Stat returns an object's identity without reading its body.
	Stat(ctx context.Context, urlPath string) (*Info, error)

	// OpenHeader returns the object with at most n leading bytes readable. n
	// must not exceed HeaderWindow. The result's Partial field reports whether
	// the body was truncated; a backend is always allowed to return the whole
	// object instead, and one that cannot serve a prefix cheaply does.
	OpenHeader(ctx context.Context, urlPath string, n int) (*Object, error)

	// Open returns the object with its whole body readable.
	Open(ctx context.Context, urlPath string) (*Object, error)

	// Describe names the origin, for startup logs and nothing else.
	Describe() string
}

// Info identifies an object well enough to build a cache key from it, without
// reading a byte of the body.
type Info struct {
	// Key is the canonical identity: the absolute path on disk for a local
	// source, the fully-joined URL for an upstream one. It is not shown to
	// clients — it names a filesystem layout or an internal host.
	Key string

	// Size is the object's length in bytes, or -1 when the origin declined to
	// say (a chunked response with no Content-Length).
	Size int64

	// Version changes whenever the object's bytes change: the modification time
	// for local disk, the ETag — else Last-Modified — for an upstream. Empty
	// means the origin offered nothing to key on, and the caller has to fall
	// back to time; see cache.Key.
	Version string
}

// Object is an open source object: its identity plus a readable body.
//
// The body is read forward only. Peek is the exception, and the reason the type
// exists rather than being a plain io.ReadCloser: the header walk needs to look
// at the first 64 KiB and then, sometimes, hand the whole object to libvips
// starting from byte zero. Peek leaves the bytes in the buffer so ReadAll still
// sees them.
type Object struct {
	Info

	// ContentType is what the origin claimed. Empty for local disk, and never
	// trusted for format detection — imagetype sniffs the bytes instead, which
	// is right both when an origin mislabels an image and when it labels an
	// HTML error page as HTML.
	ContentType string

	// Partial reports that the readable body is a prefix of the object rather
	// than all of it.
	Partial bool

	br     *bufio.Reader
	closer io.Closer
}

func newObject(info Info, r io.Reader, closer io.Closer) *Object {
	return &Object{
		Info:   info,
		br:     bufio.NewReaderSize(r, HeaderWindow),
		closer: closer,
	}
}

// Peek returns up to n leading bytes without consuming them, so a later Read or
// ReadAll still starts at byte zero. n must not exceed HeaderWindow.
//
// A short object yields a short slice and a nil error: every caller here treats
// "that is the whole file" as an ordinary case, not a failure.
func (o *Object) Peek(n int) ([]byte, error) {
	if n > HeaderWindow {
		return nil, fmt.Errorf("source: peek of %d exceeds the %d-byte window", n, HeaderWindow)
	}
	b, err := o.br.Peek(n)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return b, err
	}
	return b, nil
}

func (o *Object) Read(p []byte) (int, error) { return o.br.Read(p) }

// WriteTo streams the remaining body, so passing an object through to a client
// does not buffer it.
func (o *Object) WriteTo(w io.Writer) (int64, error) { return o.br.WriteTo(w) }

// ReadAll reads the whole body into memory, refusing at limit bytes.
//
// The limit is a real bound, not a hint: it reads one byte past it and fails if
// that byte exists, so an origin that lied in Content-Length — or never sent one
// — cannot talk iStore into an unbounded allocation. A limit of zero or less
// means no bound.
func (o *Object) ReadAll(limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(o.br)
	}

	b, err := io.ReadAll(io.LimitReader(o.br, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

func (o *Object) Close() error {
	if o.closer == nil {
		return nil
	}
	return o.closer.Close()
}
