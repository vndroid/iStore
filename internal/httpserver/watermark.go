package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/kane/istore/internal/imagedata"
	"github.com/kane/istore/internal/options"
	"github.com/kane/istore/internal/ossprocess"
	"github.com/kane/istore/internal/source"
)

// watermarkProvider supplies the watermark image the pipeline composites.
//
// imgproxy's provider is configured once at startup and returns the same image
// for every request. OSS names the watermark per request
// (`watermark,image_<base64url of an object key>`), so this one reads the key
// the parser left in the options bag and loads it from the same root the request
// itself is served from — which means the path resolver, and therefore the
// traversal and symlink checks, apply to watermarks too.
type watermarkProvider struct {
	src *source.Local

	// Watermarks repeat across requests far more than sources do — a site
	// usually has one — so they are held in memory after the first load. The
	// map is keyed by the object key and never evicted: the number of distinct
	// watermarks a deployment uses is small and bounded by what its own URLs
	// reference.
	mu     sync.RWMutex
	loaded map[string]imagedata.ImageData
}

func newWatermarkProvider(src *source.Local) *watermarkProvider {
	return &watermarkProvider{
		src:    src,
		loaded: make(map[string]imagedata.ImageData),
	}
}

// Get implements auximageprovider.Provider.
func (p *watermarkProvider) Get(_ context.Context, o *options.Options) (imagedata.ImageData, http.Header, error) {
	path := o.Main().GetString(ossprocess.KeyWatermarkPath, "")
	if path == "" {
		// No watermark requested. The pipeline treats a nil image as "skip".
		return nil, nil, nil
	}

	p.mu.RLock()
	d, ok := p.loaded[path]
	p.mu.RUnlock()
	if ok {
		// Ref so the caller's Close does not release the cached copy.
		return d.Ref(), make(http.Header), nil
	}

	name, err := p.resolve(path)
	if err != nil {
		return nil, nil, err
	}

	d, err = imagedata.NewFromFile(name)
	if err != nil {
		return nil, nil, ErrBadWatermark
	}

	p.mu.Lock()
	// Another request may have loaded it while this one was reading the file;
	// keep whichever landed first so there is exactly one cached instance.
	if existing, ok := p.loaded[path]; ok {
		p.mu.Unlock()
		d.Close()
		return existing.Ref(), make(http.Header), nil
	}
	p.loaded[path] = d
	p.mu.Unlock()

	return d.Ref(), make(http.Header), nil
}

// ErrBadWatermark reports a watermark key that names nothing servable.
//
// Missing, unreadable, outside the root and "is a directory" all collapse to
// this one error on purpose: the alternative tells a prober which paths exist,
// which is the same reason the source handler answers 404 for an escape attempt.
var ErrBadWatermark = errors.New("watermark image not found")

// resolve maps the object key through the same safety checks as a source path.
func (p *watermarkProvider) resolve(path string) (string, error) {
	name, _, err := p.src.Stat("/" + path)
	if err != nil {
		return "", ErrBadWatermark
	}
	return name, nil
}

// Close releases every cached watermark.
func (p *watermarkProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, d := range p.loaded {
		d.Close()
		delete(p.loaded, k)
	}
	return nil
}
