// Package imagedata carries an image's bytes together with its detected format.
//
// This is a much-reduced stand-in for imgproxy's package of the same name.
// imgproxy's version also covers HTTP downloading, response limits, streaming
// providers and cloud-storage transports; iStore reads from the local disk and
// from libvips memory targets only, so all of that is gone.
//
// What is kept, deliberately, is the reference counting and the cancel-hook:
// vips.Image.Save() hands back a buffer that lives inside a VipsTarget, and the
// only thing that frees that target is the cancel function attached here. Drop
// the refcounting and every conversion leaks a libvips blob.
package imagedata

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/vndroid/istore/internal/imagetype"
)

// ImageData is a refcounted holder of image bytes plus the detected format.
//
// The zero value is not usable; construct with NewFromBytes* or NewFromFile.
type ImageData interface {
	// Reader returns a fresh reader positioned at the start of the data.
	Reader() io.ReadSeeker
	// Size is the length of the data in bytes.
	//
	// The error return exists to match the shape imgproxy's processing pipeline
	// expects; iStore always holds the bytes in memory, so it is always nil.
	Size() (int, error)
	// Format is the detected image type.
	Format() imagetype.Type
	// Ref takes an additional reference. Every Ref needs a matching Close.
	Ref() ImageData
	// AddCancel attaches a cleanup function, run when the last reference closes.
	// Cancel functions must be idempotent.
	AddCancel(cancel func())
	// Close releases one reference.
	Close() error
}

type imageData struct {
	data   []byte
	format imagetype.Type

	mu       sync.Mutex
	cancel   []func()
	refCount atomic.Int32
}

func newImageData(b []byte, format imagetype.Type) *imageData {
	d := &imageData{data: b, format: format}
	d.refCount.Store(1)
	return d
}

// NewFromBytesWithFormat wraps b, trusting the caller's format.
func NewFromBytesWithFormat(format imagetype.Type, b []byte) ImageData {
	return newImageData(b, format)
}

// NewFromBytes wraps b and sniffs the format from its magic bytes.
func NewFromBytes(b []byte) (ImageData, error) {
	t, err := imagetype.Detect(bytes.NewReader(b), "", "")
	if err != nil {
		return nil, err
	}
	return newImageData(b, t), nil
}

// NewFromBase64 decodes a standard-encoded base64 string and sniffs the format.
func NewFromBase64(s string) (ImageData, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return NewFromBytes(b)
}

// NewFromFile reads path fully into memory and sniffs the format.
//
// Reading the whole file is fine here because every caller is about to hand the
// bytes to libvips anyway. The one exception is the info endpoint, which uses
// imagemeta instead and never gets this far.
func NewFromFile(path string) (ImageData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return NewFromBytes(b)
}

func (d *imageData) Reader() io.ReadSeeker  { return bytes.NewReader(d.data) }
func (d *imageData) Size() (int, error)     { return len(d.data), nil }
func (d *imageData) Format() imagetype.Type { return d.format }

// Bytes exposes the underlying slice without copying. Callers must not retain it
// past Close(): for images produced by vips.Image.Save() the memory belongs to a
// VipsTarget that Close() frees.
func (d *imageData) Bytes() []byte { return d.data }

func (d *imageData) Ref() ImageData {
	for {
		old := d.refCount.Load()
		if old <= 0 {
			panic("imagedata: Ref() on a closed ImageData")
		}
		if d.refCount.CompareAndSwap(old, old+1) {
			return d
		}
	}
}

func (d *imageData) AddCancel(cancel func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancel = append(d.cancel, cancel)
}

func (d *imageData) Close() error {
	n := d.refCount.Add(-1)
	if n < 0 {
		panic("imagedata: Close() on an already closed ImageData")
	}
	if n > 0 {
		return nil
	}

	d.mu.Lock()
	cancels := d.cancel
	d.cancel = nil
	d.data = nil
	d.mu.Unlock()

	for _, c := range cancels {
		c()
	}

	return nil
}

// Bytes returns the raw bytes of d. Package-level so it works through the
// interface without widening it for callers that never need the slice.
func Bytes(d ImageData) []byte {
	if id, ok := d.(*imageData); ok {
		return id.Bytes()
	}
	r := d.Reader()
	b, _ := io.ReadAll(r)
	return b
}
