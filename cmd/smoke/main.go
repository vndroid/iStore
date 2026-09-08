// Command smoke runs one image through the full ported processing pipeline.
// It exists to prove the port works end to end, not as a production entry point.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/vndroid/istore/internal/imagedata"
	"github.com/vndroid/istore/internal/imagetype"
	"github.com/vndroid/istore/internal/options"
	"github.com/vndroid/istore/internal/options/keys"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/security"
	"github.com/vndroid/istore/internal/vips"
)

func main() {
	vc := vips.NewDefaultConfig()
	if _, err := vips.LoadConfigFromEnv(&vc); err != nil {
		panic(err)
	}
	if err := vips.Init(&vc); err != nil {
		panic(err)
	}
	defer vips.Shutdown()

	sc := security.NewDefaultConfig()
	checker, err := security.New(&sc)
	if err != nil {
		panic(err)
	}

	pc := processing.NewDefaultConfig()
	if _, err := processing.LoadConfigFromEnv(&pc); err != nil {
		panic(err)
	}
	proc, err := processing.New(&pc, checker, nil)
	if err != nil {
		panic(err)
	}

	src, err := imagedata.NewFromFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer src.Close()
	n, _ := src.Size()
	fmt.Printf("源图 %s %d 字节\n\n", src.Format(), n)

	cases := []struct {
		name string
		set  func(*options.Options)
	}{
		{"format,avif", func(o *options.Options) { o.Set(keys.Format, imagetype.AVIF) }},
		{"format,jxl", func(o *options.Options) { o.Set(keys.Format, imagetype.JXL) }},
		{"format,webp", func(o *options.Options) { o.Set(keys.Format, imagetype.WEBP) }},
		{"format,jpg", func(o *options.Options) { o.Set(keys.Format, imagetype.JPEG) }},
		{"resize w=800 + avif", func(o *options.Options) {
			o.Set(keys.Format, imagetype.AVIF)
			o.Set(keys.Width, 800)
		}},
	}

	fmt.Printf("  %-22s %-12s %10s %9s\n", "选项", "输出尺寸", "字节", "耗时")
	for _, tc := range cases {
		o := options.New()
		tc.set(o)

		t0 := time.Now()
		res, err := proc.ProcessImage(context.Background(), src, o)
		dt := time.Since(t0)
		if err != nil {
			fmt.Printf("  %-22s 失败: %v\n", tc.name, err)
			continue
		}
		sz, _ := res.OutData.Size()
		fmt.Printf("  %-22s %-12s %10d %7.0f ms\n",
			tc.name,
			fmt.Sprintf("%dx%d", res.ResultWidth, res.ResultHeight),
			sz, float64(dt.Microseconds())/1000)
		os.WriteFile("/tmp/smoke/"+res.OutData.Format().String()+"_"+fmt.Sprint(res.ResultWidth), imagedata.Bytes(res.OutData), 0o644)
		res.OutData.Close()
	}
}
