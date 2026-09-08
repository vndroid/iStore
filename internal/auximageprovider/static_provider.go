package auximageprovider

import (
	"context"
	"errors"
	"net/http"

	"github.com/vndroid/istore/internal/imagedata"
	"github.com/vndroid/istore/internal/options"
)

// staticProvider is a simple implementation of ImageProvider, which returns
// a static saved image data and headers.
type staticProvider struct {
	data    imagedata.ImageData
	headers http.Header
}

// Get returns the static image data and headers stored in the provider.
func (s *staticProvider) Get(_ context.Context, _ *options.Options) (imagedata.ImageData, http.Header, error) {
	// Ref() increments the ref count so the caller can Close() independently
	// without releasing the shared underlying data.
	return s.data.Ref(), s.headers.Clone(), nil
}

// Close releases the static image data held by the provider.
func (s *staticProvider) Close() error {
	return s.data.Close()
}

// NewStaticProvider creates a Provider from either a base64 blob or a file path.
//
// imgproxy also accepts a URL here and downloads the watermark over HTTP, which
// is what drags its imagedata.Factory — and behind it the whole fetcher and
// storage tree, with the AWS, Azure, GCS and Swift SDKs — into the graph.
// iStore serves images off local disk, so a watermark comes from local disk too.
func NewStaticProvider(
	_ context.Context,
	c *StaticConfig,
	_ string,
) (Provider, error) {
	var (
		data    imagedata.ImageData
		headers = make(http.Header)
		err     error
	)

	switch {
	case len(c.Base64Data) > 0:
		data, err = imagedata.NewFromBase64(c.Base64Data)
	case len(c.Path) > 0:
		data, err = imagedata.NewFromFile(c.Path)
	case len(c.URL) > 0:
		return nil, errors.New("auximageprovider: loading an auxiliary image from a URL is not supported; use a local path")
	default:
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return &staticProvider{
		data:    data,
		headers: headers,
	}, nil
}
