# iStore

An image service that speaks the Alibaba Cloud OSS `x-oss-process` URL grammar,
built on libvips. The image engine is ported from
[imgproxy](https://github.com/imgproxy/imgproxy) (Apache-2.0); see `NOTICE`.
iStore itself is AGPL-3.0 — see `LICENSE`, and the License section at the end
for why those two go together.

Status: **working.** `resize`, `crop`, `indexcrop`, `trim`, `rotate`,
`auto-orient`, `blur`, `sharpen`, `pixelate`, `bright`, `contrast`, `circle`,
`rounded-corners`, `watermark`, `quality`, `format`, `interlace`, `info` and
`average-hue` are served over HTTP from a local directory, with a bounded disk cache, request coalescing,
a concurrency limit, `Accept`-based format negotiation and optional HMAC request
signing.

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

**`rotate`** — `0..360`, clockwise, as in OSS.

A multiple of 90 is a lossless transpose and keeps the frame. Any other angle is
a resample: the canvas grows to the bounding box of the rotated rectangle, and
the corners it exposes are transparent — or the background colour when the
output format has no alpha, which is what OSS does. A 300×200 source at 45°
comes out 354×354, and `rotate,45/format,jpg` has white corners.

The two take different libvips calls, and the free rotation runs *after* the
resize contract has been satisfied: `resize,m_fill,w_100,h_100/rotate,45` gives
a 141×141 result, not a 100×100 one with its corners sheared off.

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

**`pixelate`** — `1..1000`, the block edge in source pixels: `pixelate,8`
averages every 8×8 block. `1` is a no-op rather than an error, so a caller
computing the block size from a zoom level need not special-case the smallest.

> `pixelate` is an **iStore addition**, not an OSS action — OSS's effect list has
> blur, sharpen, bright and contrast but no pixelate. It is here because it is
> the one effect that reliably makes a face or a plate unreadable without
> leaving a recoverable original; a blur strong enough to do that usually ruins
> the rest of the frame.

**`trim`** — `t_0..254` threshold (default 10), `c_RRGGBB` border colour,
`eh_0|1` / `ev_0|1` equal-sides. Removes a uniform border.

Omitting `c_` detects the colour from the image's own border, which is what you
want almost always; giving it trims that colour specifically and leaves anything
else alone. `eh_1`/`ev_1` remove the *same* amount from opposite sides, keeping
the subject centred rather than flush.

> `trim` is an **iStore addition**, not an OSS action. It earns its place because
> it fixes a class of source file rather than restyling it: screenshots and
> exported logos routinely carry a band of background that no amount of resizing
> removes, and trimming at serve time avoids re-cutting the originals.

**`bright`** — `-100..100`, `0` unchanged. The value becomes an offset on the
0..255 scale (`v * 2.55`), so `bright,100` is white and `bright,-100` is black.

**`contrast`** — `-100..100`, `0` unchanged. Mapped through the standard contrast
curve `259*(c+255) / (255*(259-c))` with `c = v * 2.55`: `-100` collapses to a
flat mid-grey, `100` reaches ~129x and acts as a threshold. A plain linear
multiplier would waste the positive half — doubling contrast is barely visible,
and the interesting values all sit above 4x.

> Both are calibrations, not specifications: OSS states the ranges but not the
> units, so output will not be bit-identical to OSS. Alpha is left alone, so
> brightening a transparent PNG does not make it opaque. When both are given they
> are folded into **one** linear pass, which matters: applied separately, a
> `contrast,-100/bright,50` chain would clip twice.

**`circle`** — `r_1..4096`. Returns a `(2r+1)`-square containing the inscribed
circle. A radius past what the image can hold is clamped to the largest
inscribed circle, as in OSS — a 300×200 source with `circle,r_4096` gives
199×199, i.e. `r = (min edge - 1) / 2`.

**`rounded-corners`** — `r_1..4096`. Keeps the frame and rounds the corners; the
radius is clamped to half the shorter side, at which point the shape is a
stadium.

> Both cut transparent pixels out of the result, so the output format matters:
> PNG and WebP keep the transparency, JPEG gets the background colour (white by
> default) — OSS behaves the same way. `circle/format,auto` therefore negotiates
> to an alpha-capable format rather than JPEG. An image that was *already* partly
> transparent stays that way: the mask multiplies into the existing alpha instead
> of replacing it.
>
> The mask is arithmetic — a signed-distance field evaluated with libvips
> operations — rather than a rasterised shape, so it needs nothing from librsvg
> and gets a one-pixel antialiased arc for free.

**`watermark`** — an image, a line of text, or both side by side. Every
parameter OSS documents is accepted.

| | |
|---|---|
| `image_` | base64url of an object key |
| `text_` | base64url of the text |
| `t_0..100` | opacity, default 100 |
| `g_` | anchor, default `se` |
| `x_`/`y_0..4096` | offset from the anchored edges, default 10 |
| `voffset_-1000..1000` | offset from the centre line, for the middle row of anchors |
| `P_1..100` | scale an image watermark to a percentage of the base image |
| `fill_0|1` | tile the watermark across the whole image |
| `padx_`/`pady_0..4096` | gaps between tiles |
| `type_` | base64url of a font name, default `wqy-zenhei` |
| `color_RRGGBB` | text colour, default black |
| `size_1..1000` | text size in pixels, default 40 |
| `shadow_0..100` | drop-shadow opacity, default 0 |
| `rotate_0..360` | rotate the text |
| `order_0|1` | which of image and text comes first, default image |
| `align_0|1|2` | top / middle / bottom, default bottom |
| `interval_0..1000` | gap between image and text |

The object key is read from the same root as the image, through the same
resolver, so traversal and symlink escapes are refused there too. All four
base64 spellings (raw/padded × url/standard alphabet) are accepted.

An **image** watermark is held in memory after the first read, keyed by its
object key. A **text** watermark is rendered on every request and never cached:
its key would be made of `text_`, `type_`, `color_`, `size_`, `shadow_` and
`rotate_`, all of which the caller picks, so a loop over distinct strings would
grow that map until the process died. Rendering a line of Pango text costs a few
hundred microseconds, which is the right price for not having that.

Combinations that cannot all be honoured are refused rather than half-applied:
`y_` with `voffset_` (both are the vertical offset), `g_`/`x_`/`y_` with
`fill_1` (which has no anchor), `padx_`/`pady_` without it, `P_` without an
image, the text parameters without text, and `order_`/`align_`/`interval_`
without both.

> Two things about text cannot match OSS byte for byte. **Which font a name
> resolves to** is the host's fontconfig, not ours: OSS's identifiers are mapped
> onto family lists with a generic fallback, and an unlisted name is passed
> through so a deployment can ask for a font it has actually installed. And the
> **drop shadow's geometry** is a calibration — OSS's `shadow_` gives only its
> transparency, so the offset and blur are scaled from the font size here.

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

**`interlace`** — `0` or `1`. `1` encodes the result so a browser can paint it
progressively as it downloads.

OSS documents this for JPEG only, where it means a progressive scan order.
iStore applies it to PNG as well, where the equivalent is Adam7 interlacing:
the caller is asking for the same behaviour and the encoder has it. On any other
output format it does nothing. Absent, the process-wide
`ISTORE_JPEG_PROGRESSIVE` / `ISTORE_PNG_INTERLACED` settings apply — the action
overrides them per request rather than replacing them.

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

When the source carries EXIF, its tags are merged into the same object, as OSS
does — `Make`, `Model`, `DateTime`, `Orientation`, `LensModel`, the GPS block and
so on. Tags are read from JPEG's `APP1`, PNG's `eXIf` chunk and WebP's `EXIF`
chunk; an image without EXIF returns exactly the eight fields above.

> Two deliberate differences, both about *formatting* rather than which tags
> appear. Values are rendered from the raw EXIF types — `GPSLatitude` is
> `"39/1 54/1 2668/100"`, where OSS runs the same tags through exiv2's
> pretty-printer and says `"39deg 54' 26.68\""`. And tags with no standard name,
> along with `UNDEFINED`-typed ones such as `MakerNote`, are skipped rather than
> emitted as hex blobs.

**`average-hue`** — terminal, like `info`, and the response is **plain text**,
not JSON, because that is what OSS returns:

```
0x285ac8
```

Unlike `info` this needs the pixels, so it decodes the image — one decode, no
resize and no encode, for a seven-byte body. Only the first frame of an
animation is read.

> **Transparency is weighted, not ignored.** A logo on a transparent field is
> stored as mostly transparent *black*, and a flat mean of those pixels answers
> "black" for an image a person would call green. iStore premultiplies and
> divides by the summed alpha, so a green mark on transparency reports green. For
> a fully opaque image the two are identical, so this only changes the answer
> where the flat mean was wrong.

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
| `ISTORE_KEY` | *(unset — no signing)* | hex HMAC key, comma-separated for rotation |
| `ISTORE_SALT` | *(unset — no signing)* | hex HMAC salt, one per key |
| `ISTORE_SIGNATURE_SIZE` | `32` | bytes of the HMAC kept, 1..32 |
| `ISTORE_TRUSTED_SIGNATURES` | *(unset)* | signature strings accepted verbatim |
| `ISTORE_LOG_LEVEL` | `info` | debug / info / warn / error |

Every `ISTORE_*` variable imgproxy documents as `IMGPROXY_*` for the processing
and security layers also applies; the prefix is the only change.

### Request signing

Off by default. Set **both** `ISTORE_KEY` and `ISTORE_SALT` (hex-encoded) and
every request except `/healthz` must carry a valid `x-istore-signature`, or it
gets `403` with `{"Code":"AccessDenied","Message":"Forbidden"}`. With either one
unset, nothing is checked and the server says so once at startup.

```sh
ISTORE_KEY=$(openssl rand -hex 32) ISTORE_SALT=$(openssl rand -hex 32) istore
```

What is signed is the object path with the process chain appended exactly as it
appears in the URL, and nothing else:

```
/photo.jpg
/photo.jpg?x-oss-process=image/resize,w_800
```

Both halves have to be in it. Signing the path alone would let anyone rewrite
the transform on a URL they were handed, and an unbounded chain of resizes is a
cheap way to spend someone else's CPU; signing the chain alone would let them
point it at a different object. Other query parameters are outside the
signature on purpose — cache-busters and analytics tags get added by things
that do not have the key.

`cmd/sign` computes it, reading the same two variables:

```sh
go build -o sign ./cmd/sign
SIG=$(ISTORE_KEY=$KEY ISTORE_SALT=$SALT ./sign '/photo.jpg?x-oss-process=image/resize,w_800')
curl "http://localhost:8080/photo.jpg?x-oss-process=image/resize,w_800&x-istore-signature=$SIG"
```

The chain has to match byte for byte, including the order of its own
parameters, because that string is what was signed.

Two details worth knowing. `ISTORE_KEY` and `ISTORE_SALT` are comma-separated
lists, and **any** configured pair validates, so a new key can be rolled out
before the old one is retired. And the rejection body is deliberately the same
for a missing signature, a wrong key and a signature issued for another path —
the real reason goes to the log at debug level, not to the client.

imgproxy signs in the URL path, because its path *is* the request. iStore's path
is the object key and nothing else, so the signature is a query parameter rather
than a segment spliced in front of a real filename.

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

10. **Six operations added to the cgo layer**, in `internal/vips/vips.c` with
    their bindings in `vips.go`. imgproxy has none of them.

    | | |
    |---|---|
    | `vips_linear_go` | linear transform of the colour bands, alpha untouched — `bright`, `contrast` |
    | `vips_round_corners_go` | rounded-rectangle alpha mask — `circle`, `rounded-corners` |
    | `vips_rotate_go` | arbitrary-angle rotation with a transparent background — `rotate` |
    | `vips_text_go` | Pango text as a coloured RGBA image, with an optional drop shadow — `watermark,text_` |
    | `vips_ensure_alpha_go` | add an opaque alpha band, so embedding leaves a transparent margin |
    | `vips_average_hue_go` | per-band mean in one pass, alpha-weighted — `average-hue` |

    Three matching pipeline steps — `processing.adjust`, `processing.rotateFree`
    and `processing.roundCorners` — sit between `cropToResult` and `fixSize`.

11. **EXIF reading added to `internal/imageinfo`.** imgproxy has no info
    endpoint; the TIFF/IFD walk that answers `image/info` is iStore's.

12. **`vips.Image.Save` gained a `SaveOverrides` parameter.** imgproxy's save
    options are all process-wide; OSS's `interlace` is per URL. The override
    struct carries just that one field, so `internal/vips` still does not import
    the options bag it was decoupled from in (2).

## Build requirements

- Go 1.24+
- libvips 8.13+ with, at minimum: libjpeg, libpng, libwebp (8.16+ only if you
  need to *read* animated JPEG XL — see "Not built yet")
- for `watermark,text_`: libvips built with Pango, plus fonts installed on the
  host — including a CJK face if the text will be Chinese, or Pango renders
  boxes
- for AVIF output: libheif built with an AV1 **encoder** (aom, SVT-AV1 or rav1e)
- for JPEG XL output: libjxl

Alpine 3.23 (`vips` 8.17.3, `go` 1.25.10, `libheif` 1.23.0):

```sh
apk add --no-cache \
  build-base pkgconf go \
  vips-dev vips-heif vips-jxl \
  font-wqy-zenhei font-noto-cjk
```

Runtime only, without the toolchain:

```sh
apk add --no-cache vips vips-heif vips-jxl font-wqy-zenhei
```

**`vips-heif` and `vips-jxl` are not optional extras.** Alpine builds libvips
with those two codecs as *dynamic modules* in their own packages, so
`vips-dev` alone gives a libvips that passes startup and then fails the first
`format,avif` request with "cannot be produced by this build of libvips".

The AVIF encoder, on the other hand, needs nothing extra: Alpine's libheif is
built against `aom-dev` with no plugin flag, so aomenc is linked in — unlike
Ubuntu, where it is a separate package.

The fonts matter only for `watermark,text_`. `font-wqy-zenhei` is worth
preferring because it is the face OSS names by default, so `type_` resolves to
the same font iStore's mapping expects; `font-noto-cjk` is a large package and
can be dropped if the text will never be Chinese.

A multi-stage image:

```dockerfile
FROM alpine:3.23 AS build
RUN apk add --no-cache build-base pkgconf go vips-dev vips-heif vips-jxl
WORKDIR /src
COPY . .
RUN CGO_ENABLED=1 go build -o /out/istore ./cmd/istore

FROM alpine:3.23
RUN apk add --no-cache vips vips-heif vips-jxl font-wqy-zenhei
COPY --from=build /out/istore /usr/local/bin/istore
ENV ISTORE_ROOT=/srv/images ISTORE_BIND=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["istore"]
```

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

`pixelate` on a 160×160 image of per-pixel random noise, counting distinct
colours in the output — the block count should be exactly (160/n)²:

```
n        distinct colours   expected   distinct in a 16x16 corner
(none)             25,584     25,600                          256
2                   6,397      6,400                           64
4                   1,595      1,600                           16
8                     399        400                            4
16                     98        100                            1
40                     16         16                            1
```

`trim` on a 300×200 image with a 120×80 red block at (40,30) — margins of
40 left, 140 right, 30 top, 90 bottom:

```
(none)                  300x200
trim                    120x80    border detected and removed entirely
trim,t_0                120x80    a flat border needs no tolerance
trim,c_FFFFFF           120x80    the same border, named explicitly
trim,eh_1               220x80    40 off each side, the smaller margin
trim,ev_1               120x140   30 off top and bottom
trim,eh_1,ev_1          220x140
```

On a *black*-bordered copy of the same image, `trim` still gives 120×80 while
`trim,c_FFFFFF` correctly does nothing (300×200) — the explicit-colour path
really uses the colour it was given. On a copy whose border carries ±6 noise,
`trim,t_0` cannot cut (300×200) and `trim,t_20` can (120×80).

Chained: `trim/resize,w_60/format,avif` gives a 60×40 AVIF.

`interlace` read back from the encoded bytes — the JPEG `SOF` marker and the
PNG `IHDR` interlace byte:

```
format,jpg                    baseline (SOF0)      830 B
interlace,0/format,jpg        baseline (SOF0)      830 B
interlace,1/format,jpg        PROGRESSIVE (SOF2)  1068 B
format,png                    none                 851 B
interlace,1/format,png        ADAM7               1100 B
interlace,1/format,webp       encodes fine, no such thing in the format
```

`average-hue` against images whose mean is known by construction:

```
solid (120,60,180)                 0x783cb4   exact
half red | half blue               0x800080   (127.5, 0, 127.5) rounded
greyscale L=200                    0xc8c8c8
half transparent | half green      0x00ff00   the transparent half does not vote
a red square over 25% of a clear canvas   0xff0000
uniform blue at alpha 128          0x0000ff
fully transparent                  0x000000
```

Both queries are terminal: `info/format,png`, `resize,w_10/average-hue`,
`average-hue,r_1` and `info/average-hue` all return 400.

`rotate` on a 300×200 source. Every angle lands on the bounding box the
trigonometry predicts, and only the multiples of 90 keep the frame:

```
rotate,0    300x200 RGB     rotate,90   200x300 RGB
rotate,30   360x323 RGBA    rotate,45   354x354 RGBA
rotate,70   291x350 RGBA    rotate,359  303x205 RGBA
```

Corners come out `a=0` on PNG and white on JPEG, `rotate,45/format,auto` with an
AVIF `Accept` returns AVIF **RGBA** where the same request without the rotation
returns RGB, and `resize,m_fill,w_100,h_100/rotate,45` gives 141×141 — the crop
happens first, so the rotation is not sheared off.

`watermark`, measured on a 600×400 canvas by which quadrant the ink lands in and
by the centroid of each layer:

```
default (se, 10px)      ink only in SE       g_nw               ink only in NW
g_center,voffset_-150   ink moves to the top row
P_50                    1172 -> 30297 ink px  (the logo scaled to half the base)
fill_1                  29300 px in each of the four quadrants — even tiling
fill_1,padx_100,pady_100 2724 px per quadrant — the gaps take effect
t_20                    same coverage, visibly faint
```

Text: `text_` renders through Pango — Latin and CJK both — and `size_`,
`color_`, `shadow_` and `rotate_` each change the pixels as asked. Image + text:
`order_0` puts the logo at x≈40 and the text at x≈147; `order_1` swaps them to
x≈182 and x≈67. With a 12px text beside a 40px logo, `align_0/1/2` place the text
at rows 10–22, 23–35 and 37–49 against the logo's 15–44 — top, middle, bottom.

Twelve malformed watermark URLs return 400 with the real reason, including a
base64 object key of `../../etc/passwd`, which is refused as "watermark image not
found" rather than confirming what does exist.

`info` on a JPEG carrying a full EXIF block returns 34 fields — the 8 basic ones
plus `Make`, `Model`, `Software`, `DateTime`, `Orientation`, `ExposureTime`,
`FNumber`, `ISOSpeedRatings`, `LensModel`, the GPS block and the rest. The same
EXIF written into a PNG (`eXIf`) and a WebP (`EXIF` chunk) returns the same 34
fields, so all three container paths agree. `UserComment`, an `UNDEFINED` tag, is
skipped. An image with no EXIF returns exactly 8 fields, byte-identical to what
it returned before EXIF merging existed.

`circle` and `rounded-corners` on a 300×200 solid blue source, reading the alpha
channel of the PNG result:

```
circle,r_50           101x101   centre a=255, corner a=0, edge pixel a=127
circle,r_4096         199x199   clamped to (min edge - 1) / 2 = 99, as OSS does
rounded-corners,r_30  300x200   corner a=0, mid-edge a=127, centre a=255
rounded-corners,r_4096 300x200  clamped to half the shorter side: a stadium,
                                so the top edge midpoint stays a=255
```

The `a=127` readings are the antialiased arc: exactly one pixel wide, at the
radius, in both shapes. On a source that is already 50% transparent, `circle`
leaves the inside at `a=128` rather than pushing it to 255 — the mask multiplies.
Output format follows: `circle,r_40/format,auto` with `Accept: image/avif,...`
returns AVIF RGBA; the same request without `circle` returns AVIF RGB. With
`format,jpg` the outside is white. On a 3-frame animated GIF with
`ISTORE_MAX_ANIMATION_FRAMES=20`, `circle,r_20/format,webp` gives a 3-frame
41×41 WebP with every frame masked.

`bright` and `contrast` on a 256×64 image holding a full 0..255 grey ramp,
sampling x = 0, 64, 128, 192, 255:

```
(none)              [  0,  64, 128, 192, 255]
bright,20           [ 51, 115, 179, 243, 255]   +51 = 20 * 2.55
bright,100          [255, 255, 255, 255, 255]
bright,-100         [  0,   0,   0,   0,   0]
contrast,0          [  0,  64, 128, 192, 255]   byte-identical to no action
contrast,-100       [127, 127, 127, 127, 127]
contrast,50         [  0,   0, 128, 255, 255]
bright,20/contrast,20 [ 0,  83, 179, 255, 255]
```

A 16-bit grey source gives the same numbers, because the step converts to 8-bit
sRGB before applying the pivot. On an RGBA source, `bright,50` and `contrast,50`
both leave alpha at 128.

`pixelate,1` returns bytes identical to no pixelate at all (same SHA-256 of the
decoded pixels). Chained: `crop,w_120,h_80,g_nw/pixelate,20` gives 120×80 with
exactly 24 colours, i.e. 6×4 blocks.

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

`go test ./internal/...` covers the grammar, the path resolver, the header
parsers, the cache evictor, the watermark provider's caching rule and the
signature. Those packages are pure logic and need no libvips, which is
deliberate — see the note on `Validate` vs `CheckEncoders` below. The
pixel-level checks above are not in there: they need a live libvips and a real
image, and they were run by hand.

CI (`.github/workflows/ci.yml`) builds and runs those tests on Alpine 3.23 with
`vips-heif` and `vips-jxl` installed, plus a `gofmt` and `go mod tidy` check. A
cgo project needs the build itself to be the test — "it compiles here" is a
claim about one libvips, not about Go.

## Not built yet

Every action in OSS's own list is implemented. What is left is not an action.

**Not an action**

- `style/<name>` — OSS's saved presets, defined in its console. Rejected with an
  explicit message rather than ignored. Would need a config file mapping a name
  to a chain.
- `padding`, `extend` and focus-point gravity are imgproxy features with no OSS
  spelling. `extend` is already reachable through `resize,m_pad`; the other two
  would need an iStore-invented action name.

**Version-dependent**

- **Animated JPEG XL needs libvips 8.16.** `jxlload` gained animation — and with
  it the `page`/`n` load options — in 8.16. `vips_jxlload_source_go` passes them
  only when built against 8.16 or newer; on older libvips it loads a JXL as a
  single frame, which is self-consistent because that loader also sets no page
  metadata, so `IsAnimated()` stays false and the animated path is never taken.
  Still images and JXL output are unaffected either way.

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

## License

iStore is licensed under the **GNU Affero General Public License, version 3**
(`LICENSE`). It is a network service, so section 13 is the clause that matters:
modify it and let other people use it over a network, and you owe them the
modified source.

The image engine is imgproxy's, under Apache-2.0 (`LICENSE-imgproxy`).
Apache-2.0 is one-way compatible with the GPL family at version 3, so a work
built from it may be distributed under AGPL-3.0 — the compatibility does not
run the other way, and nothing here relicenses imgproxy itself. libvips is
LGPL-2.1 *or later*, and it is that "or later" which makes the link lawful: it
allows LGPL-3.0, which is compatible with AGPL-3.0. `NOTICE` has the full
attribution, the file-by-file provenance and the third-party list.
