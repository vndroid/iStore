# iStore

An image service that speaks the Alibaba Cloud OSS `x-oss-process` URL grammar,
built on libvips. The image engine is ported from
[imgproxy](https://github.com/imgproxy/imgproxy) (Apache-2.0); see `NOTICE`.

Status: **working.** `resize`, `crop`, `indexcrop`, `rotate`, `auto-orient`,
`blur`, `sharpen`, `watermark`, `quality`, `format` and `info` are served over
HTTP from a local directory, with a bounded disk cache, request coalescing,
a concurrency limit and `Accept`-based format negotiation.

## Usage

```sh
ISTORE_ROOT=/srv/images \
ISTORE_CACHE_DIR=/var/cache/istore \
ISTORE_BIND=127.0.0.1:8080 \
  istore
```

```
GET /photo.jpg                                    the source, untouched
GET /photo.jpg?x-oss-process=image/format,avif
GET /photo.jpg?x-oss-process=image/resize,w_800
GET /photo.jpg?x-oss-process=image/resize,m_fill,w_200,h_200/quality,q_80/format,avif
GET /photo.jpg?x-oss-process=image/info           JSON metadata
GET /healthz
```

### Supported actions

**`resize`** — `m_` mode, `w_`/`h_` box, `l_` longest side, `s_` shortest side,
`p_` percent, `limit_0|1`, `color_RRGGBB`.

| mode | meaning |
|---|---|
| `lfit` *(default)* | fit inside w×h, keep aspect |
| `mfit` | cover w×h without cropping, so one side overflows |
| `fill` | cover w×h then centre-crop to exactly w×h |
| `pad` | fit inside w×h then pad to exactly w×h with `color_` (default white) |
| `fixed` | force exactly w×h, ignoring aspect |

`limit_1` is the default and matches OSS: if honouring the request would enlarge
the source, the source is returned unchanged. `limit_0` allows upscaling.

**`crop`** — `w_`/`h_` size, `x_`/`y_` offset, `g_` anchor
(`nw` *(default)* `north` `ne` `west` `center` `east` `sw` `south` `se`).
A crop larger than the image is clamped to the image, as in OSS.

**`rotate`** — `0` `90` `180` `270` `360`, clockwise. OSS accepts any angle
0–360; libvips only rotates losslessly by multiples of 90, and anything else is
refused rather than silently rounded.

**`auto-orient`** — `0` or `1`: whether to apply the EXIF orientation tag.
Default 1, same as OSS.

**`blur`** — `r_1..50`, `s_1..50`. Both are required, as in OSS, but only `s_`
(the standard deviation) reaches the pipeline: libvips' gaussian blur takes a
sigma and derives its own kernel radius, so `r_` is accepted for URL
compatibility and does not change the output.

**`indexcrop`** — `x_<size>,i_<index>` or `y_<size>,i_<index>`. Cuts the image
into slices of `size` pixels along one axis and keeps the `index`-th, 0-based.
Despite the name, `x_`/`y_` are the slice width/height, not a count. A final
short slice is kept rather than discarded, as in OSS.

**`sharpen`** — `50..399`. OSS does not say what the number means; the pipeline
takes a gaussian sigma, so this maps `v/100`, putting OSS's recommended 100 at
sigma 1. A calibration, not a specification — output will not be bit-identical
to OSS.

**`watermark`** — `image_<base64url of an object key>`, `t_0..100` opacity,
`g_` anchor (default `se`), `x_`/`y_` offsets. The watermark is read from the
same root as the image, through the same resolver, so traversal and symlink
escapes are refused there too. All four base64 spellings (raw/padded ×
url/standard alphabet) are accepted.

> `text_` watermarks are **not** supported. Rendering text needs a text engine;
> libvips has one via Pango but imgproxy's binding does not expose it, so the
> parser refuses `text_` rather than silently dropping it.

**`format`** — `jpg` `jpeg` `png` `webp` `gif` `avif` `heic` `jxl` `tiff` `bmp`,
plus **`auto`**.

`format,auto` is an iStore addition, not an OSS action. It picks the best format
the client's `Accept` header lists — AVIF, then WebP, then the source's own
format — and sets `Vary: Accept`. It exists because the alternative is a
`<picture>` element with an AVIF `<source>` and a fallback, hand-written at every
call site and stale the moment browser support moves.

Wildcards are ignored on purpose: every browser sends `*/*`, and treating that
as "AVIF is fine" would send AVIF to clients that cannot read it. JPEG XL is not
a candidate — at ~15% support it would mean encoding a format almost nobody can
read, and it is the slowest of the three to produce. Ask for it explicitly.

**`quality`** — `Q_1..100` (absolute) or `q_1..100`.

> **`q_` is an approximation.** In OSS, `q_n` is *relative*: for JPEG it means n
> percent of the source's own quality, while `Q_n` is absolute. Recovering a
> source's quality means reading back its quantisation tables, which libvips does
> not expose, and rejecting `q_` would break the spelling most OSS URLs use. So
> iStore treats both as absolute. For a source saved near iStore's default
> quality the two agree closely; for a heavily-compressed source, `q_n` here
> produces a larger file than OSS would.

**`info`** — terminal, cannot be chained with transforms (same rule as OSS).

`image/info` returns the OSS shape, every value a string:

```json
{
  "FileSize": {"value": "17947"},
  "Format": {"value": "jpg"},
  "FrameCount": {"value": "1"},
  "ImageHeight": {"value": "267"},
  "ImageWidth": {"value": "400"},
  "ResolutionUnit": {"value": "1"},
  "XResolution": {"value": "1/1"},
  "YResolution": {"value": "1/1"}
}
```

Errors use the OSS envelope, so a client written against OSS parses them
unchanged:

```json
{"Code": "InvalidArgument", "Message": "unsupported format \"tga\""}
```

### Configuration

| variable | default | meaning |
|---|---|---|
| `ISTORE_ROOT` | *(required)* | directory images are served from |
| `ISTORE_CACHE_DIR` | *(unset — no cache)* | where transcoded results are stored |
| `ISTORE_BIND` | `:8080` | listen address |
| `ISTORE_CONCURRENCY` | `GOMAXPROCS` | simultaneous encodes |
| `ISTORE_MAX_SOURCE_BYTES` | `104857600` | reject larger sources |
| `ISTORE_PROCESS_TIMEOUT_MS` | `20000` | per-request processing deadline |
| `ISTORE_CACHE_CONTROL` | `public, max-age=31536000, immutable` | response header |
| `ISTORE_CACHE_MAX_BYTES` | `0` *(unbounded)* | evict least-recently-used entries above this |
| `ISTORE_CACHE_MAX_AGE_HOURS` | `0` *(no age bound)* | evict entries untouched for longer |
| `ISTORE_CACHE_EVICT_INTERVAL_MIN` | `10` | how often to sweep |
| `ISTORE_AVIF_SPEED` | `8` | 0 slowest/smallest .. 9 fastest/largest |
| `ISTORE_LOG_LEVEL` | `info` | debug / info / warn / error |

Every `ISTORE_*` variable imgproxy documents as `IMGPROXY_*` for the processing
and security layers also applies; the prefix is the only change.

## Why not just run imgproxy

imgproxy already does the transcoding well, but two things don't fit:

- its URL grammar is its own, not `x-oss-process`
- its `/info` endpoint is a **Pro (paid)** feature

So iStore keeps imgproxy's libvips layer, which is the part that took years to
get right, and replaces the URL front-end and the info endpoint.

## What was ported, and what was left behind

The whole imgproxy `processing` package is here — resize, crop, gravity, trim,
padding, extend, watermark, filters, colour management, metadata stripping.
What did *not* come with it is the dependency tree.

| | internal packages | Go lines | third-party modules |
|---|---:|---:|---:|
| imgproxy's `processing` closure | 56 | 25,271 | 25 |
| **iStore** | **19** | **8,208** | **1** |

The 25 modules include the AWS SDK, Azure SDK, Google Cloud Storage, OpenStack
Swift, OpenTelemetry, Datadog, New Relic, Sentry, Airbrake, Bugsnag, Prometheus
and gopsutil. None of them are needed to process an image. They arrive
transitively along exactly two edges:

    processing -> server    -> monitoring -> every observability backend
    processing -> imagedata -> fetcher    -> storage -> every cloud backend

Cut those two edges and the whole thing collapses. `processing` touches `server`
at three call sites, all of them `server.CheckTimeout(ctx)`; that is now
`internal/timeout`. `imagedata` was rewritten small. The one module left is
`github.com/trimmer-io/go-xmp`, which `processing` genuinely uses to preserve
XMP metadata.

### Ported from imgproxy

```
internal/processing/     the pipeline, in full
internal/options/        typed option bag used as the pipeline's IR
internal/security/       source-image limits (max resolution, max frames, ...)
internal/vips/           libvips cgo bindings (1,307 Go + 3,770 C)
internal/imagetype/      format registry and magic-byte detection
internal/imagemeta/      IPTC and Photoshop metadata blocks
internal/auximageprovider/  watermark source
internal/env/            typed env-var descriptors
internal/bufreader/  internal/errctx/  internal/ioutil/  internal/imath/  internal/ensure/
```

### Written for iStore

```
internal/imagedata/      refcounted bytes+format holder
internal/timeout/        the one function iStore needed from imgproxy's server package
```

### What was changed relative to imgproxy

1. **`imagedata` rewritten small.** Kept: reference counting and the cancel
   hook — not optional, since `vips.Image.Save()` returns a buffer owned by a
   `VipsTarget` and the attached cancel function is the only thing that frees
   it. Dropped: HTTP download, response limits, streaming providers, and the
   cloud-storage transports behind them.

2. **`vips` no longer imports `imagedata` and `options`.** Coupling was 8 call
   sites. `vips.Image.Save()` lost its `*options.Options` parameter, which
   imgproxy declared but never read (`newSaveOptions(_ *options.Options)`).

3. **`env` lost `aws.go`, `gcp.go`, `load.go`.** Those resolve config values
   from AWS Secrets Manager, AWS SSM and GCP Secret Manager, and are the sole
   reason three cloud SDKs appear in the graph. The typed-descriptor core is
   unchanged.

4. **`server.CheckTimeout` → `internal/timeout.Check`.** Three call sites.

5. **`options/parser` removed.** That is imgproxy's own URL grammar; iStore
   parses `x-oss-process` instead. This also drops `clientfeatures`.

6. **SVG removed** — both `imagetype/svg.go` (detection) and `processing/svg`
   (sanitizing). Both need imgproxy's `xmlparser`, which needs
   `golang.org/x/net/html/charset`. iStore does not rasterize or serve vectors,
   and passing SVG through *unsanitized* would be worse than refusing it, so
   `skipStandardProcessing` returns an explicit error for SVG input.

7. **Watermarks load from disk only.** imgproxy's `NewStaticProvider` also
   accepts a URL and downloads it, which is what pulls in `imagedata.Factory`
   and the fetcher/storage tree.

8. **Env prefix `IMGPROXY_*` → `ISTORE_*`** (30 variables).

9. **Tests not carried over.** They depend on `testify` and a large `testdata`
   tree.

## Build requirements

- Go 1.24+
- libvips 8.13+ with, at minimum: libjpeg, libpng, libwebp
- for AVIF output: libheif built with an AV1 **encoder** (aom, SVT-AV1 or rav1e)
- for JPEG XL output: libjxl

Ubuntu 24.04:

```sh
apt-get install -y libvips-dev libheif-plugin-aomenc libheif-plugin-dav1d
```

The stock `libvips-dev` on Ubuntu has HEIF *decode* only; without
`libheif-plugin-aomenc` an AVIF save fails with `heifsave: Unsupported compression`.

macOS:

```sh
brew install vips        # includes libheif with aom, and libjxl
```

Verify the encoders are actually present before trusting a build:

```sh
vips copy input.png /tmp/probe.avif && echo AVIF ok
vips copy input.png /tmp/probe.jxl  && echo JXL ok
```

## Measured

`cmd/smoke` runs one image through the full pipeline. On 2 vCPU, a 2400×1350
photo (source JPEG q85, 130,340 bytes):

```
选项                     输出尺寸        字节      耗时
format,avif            2400x1350     31,697    149 ms
format,jxl             2400x1350     63,198    546 ms
format,webp            2400x1350     45,402    221 ms
format,jpg             2400x1350    102,136     18 ms
resize w=800 + avif    800x450       11,637     58 ms
```

The last row is not on the roadmap below — resize came along with the port and
already works. Only the URL grammar for it is missing.

AVIF speed is controlled by `ISTORE_AVIF_SPEED` (0 slowest/smallest ..
9 fastest/largest, default 8). Measured with `avifenc` on the same machine and
image, for calibration:

```
-s 0   7.56 s   22 KB
-s 4   2.08 s   24 KB
-s 6   0.38 s   25 KB     <- size/time knee
-s 8   0.19 s   41 KB
-s 10  0.14 s   63 KB
```

Every conversion result must be cached; at 149 ms a page of thumbnails would
otherwise saturate a small box.

## Verified end to end

Against a running server, source `t.jpg` 2400×1350:

```
info (jpg/png/gif/webp)     dimensions match Pillow exactly; GIF frame count too
info (avif)                 2400x1350 via the libvips fallback
format,avif|jxl|webp|png|jpg  200, correct Content-Type, decodes to 2400x1350
cache                       MISS 223ms -> HIT 1.4ms
singleflight                12 concurrent identical requests -> 1 MISS + 11 COALESCED,
                            1 cache entry (not 12 encodes)
HEAD                        200, no body;  POST -> 405
```

Resize, against a 400×300 source, checking the decoded output not just the status:

```
m_lfit,w_200,h_200            -> 200x150     m_mfit,w_200,h_200   -> 267x200
m_fill,w_200,h_200            -> 200x200     m_pad,w_200,h_200    -> 200x200
m_fixed,w_200,h_200           -> 200x200     w_200                -> 200x150
h_150                         -> 200x150     l_200                -> 200x150
s_150                         -> 200x150     p_50                 -> 200x150
p_200                         -> 400x300     p_200,limit_0        -> 800x600
m_lfit,w_800,h_600            -> 400x300     ...,limit_0          -> 800x600
w_200/format,avif             -> 200x150 image/avif
```

The two `limit` rows are the OSS default in action: asking for something bigger
than the source returns the source untouched.

Pad fills with the requested colour (`color_FF8000` gives a `(255,127,0)`
border), and quality is monotonic: `Q_30` 7,423 B, `Q_60` 11,016 B,
`Q_90` 30,425 B.

Crop and rotate are checked by pixel, not by status code, against a 400×300
image painted in four quadrants — red top-left, green top-right, blue
bottom-left, yellow bottom-right:

```
crop,w_200,h_150             -> all red      (nw is the default anchor)
crop,w_200,h_150,g_ne        -> all green
crop,w_200,h_150,g_sw        -> all blue
crop,w_200,h_150,g_se        -> all yellow
crop,w_200,h_150,x_200       -> all green    (x_/y_ are absolute pixels)
crop,w_200,h_150,y_150       -> all blue
crop,w_200,h_150,x_200,y_150 -> all yellow
crop,w_200,h_150,g_center    -> all four quadrants, as the centre box straddles them

rotate,90   300x400, red moves top-left -> top-right   (clockwise, like OSS)
rotate,180  400x300, red moves top-left -> bottom-right
rotate,270  300x400, red moves top-left -> bottom-left
```

Blur is monotonic, measured as the colour difference across the hard quadrant
boundary: unblurred 510, `s_8` 160, `s_25` 52.

A full chain — `crop,w_200,h_150,g_se/resize,w_100/format,avif` — returns a
100×75 AVIF of 581 bytes whose centre pixel is `(255,255,2)`, i.e. the yellow
quadrant, scaled and re-encoded.

`indexcrop` on the same 400×300 quadrant image:

```
indexcrop,x_200,i_0  -> 200x300, left half   (red over blue)
indexcrop,x_200,i_1  -> 200x300, right half  (green over yellow)
indexcrop,y_150,i_1  -> 400x150, bottom half (blue and yellow)
indexcrop,x_150,i_2  -> 100x300               (the short final slice, kept)
indexcrop,x_200,i_5  -> 400 "i is 5 but the image only has 2 slice(s) of 200 px"
```

`watermark` with an 80×40 magenta logo lands in the anchored corner
(`g_se`/`g_nw`/`g_center` all verified by scanning for magenta in that region),
and opacity blends: over the yellow quadrant, `t_20` gives `(255,215,40)` and
`t_100` gives `(255,54,200)`. A watermark key of `../../etc/passwd`, a symlink
out of the root, and a missing file all return the same 400 —
`watermark image not found` — so the endpoint cannot be used to probe the
filesystem.

`format,auto`:

```
Accept: image/avif,image/webp,*/*   -> image/avif   Vary: Accept
Accept: image/webp,*/*              -> image/webp   Vary: Accept
Accept: */*                         -> image/png    (the source format)
Accept: (absent)                    -> image/png
```

Three distinct capabilities produced three cache entries; the last two share one.

Cache eviction, ten entries totalling 438,603 bytes with the largest at 88,298:
a 200,000-byte bound left two entries and 164,834 bytes. A 60,000-byte bound
emptied the cache, which is correct — the largest single entry exceeds that
budget on its own.

Path safety, all returning 404 with no content leaked:

```
/../etc/passwd            /..%2f..%2fetc%2fpasswd    /sub/../../etc/passwd
/etc/passwd               /  (directory)
/escape.jpg               (a symlink inside the root pointing at /etc/passwd)
```

Argument errors, all 400 with an OSS envelope: `image/info,x_1`,
`image/format`, `image/format,tga`, `image/info/format,avif`,
`image/resize,w_100`, `video/info`.

`go test ./internal/...` covers the grammar, the path resolver and the header
parsers. Those three packages are pure logic and need no libvips, which is
deliberate — see the note on `Validate` vs `CheckEncoders` below.

## Not built yet

- `pixelate` and `trim`: the pipeline has both, they need only a parameter
  translation in `ossprocess.Chain.Apply`, the same shape as `resize.go`.
- `circle`, `rounded-corners`, `bright`, `contrast`: no equivalent in the
  pipeline. These need new libvips calls exposed through `internal/vips`.
- `watermark,text_`: needs a text renderer, see above.
- Request signing. `internal/security` carries imgproxy's implementation and it
  is wired to `ISTORE_KEY` / `ISTORE_SALT`, but nothing calls it: with a local
  root and a path-traversal-safe resolver there is no URL to forge. That changes
  the day a remote source is added.

## Two design notes worth keeping

`ossprocess.Validate()` checks grammar only. Whether libvips can actually
*encode* a format is `CheckEncoders()`, called separately after `vips.Init()`.

They are split because `vips.SupportsSave()` calls into the C library, and
libvips aborts the process with `SIGABRT` if `vips_init` has not run — not an
error return, a crash. A syntax check that touched it could not be unit-tested,
and any code path reaching it before startup would take the process down. The
test suite found this the first time it ran.

**`resize` resolves against the source, not in the parser.** Three of OSS's
parameters cannot become a scale factor without knowing the source size: `m_mfit`
and `s_` constrain the *smaller* output side, which depends on the aspect ratio,
and `limit_1` is a comparison against the source. So `ossprocess.Resize` is a
description, and `Resolve(srcW, srcH)` produces the concrete target.
`Chain.NeedsSourceSize()` lets the server skip the header read for chains that
do not need it — `format,avif` alone, the common case, still costs nothing.

That also explains `m_mfit` mapping to `ResizeForce` rather than `ResizeFill`:
imgproxy's Fill scales to cover *and then crops*, which is `m_fill`. For `m_mfit`
the covering size is computed here and forced, so nothing is cropped.

## A note on JPEG XL

`jxlsave` works and is wired up, but browser support is 14.63% globally
(caniuse): Chrome 145–155 has it behind a flag, **Edge does not support it**,
Firefox does not, Safari 17+ is partial. Serve it only via `Accept` negotiation,
and expect near-zero traffic.
