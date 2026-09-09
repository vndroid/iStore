# Differences from Alibaba Cloud OSS

iStore answers the OSS `x-oss-process` grammar, but it is not OSS. This file is
the checklist of where the two part company, so a service migrating onto iStore
can find out here rather than in production.

Everything below was measured against a running server, not read off the source.
Where a row says "OSS", it is what the OSS documentation states — linked at the
bottom — not a live comparison against the service.

**Read this first if you are migrating:** the rows most likely to change a
response your callers already parse are [`info` resolution fields](#info) and
[BMP transparency](#format-conversion). The pixel budget matches OSS; the
[single-dimension limits](#limits-and-defaults) do not.

---

## Not implemented

| Action | Behaviour |
|---|---|
| `style/<name>` | `400`. OSS's saved presets live in its console; iStore would need a config file mapping a name to a chain. Rejected explicitly rather than ignored, so a caller cannot mistake it for a no-op. |

Every action in OSS's own list is implemented: `resize`, `crop`, `indexcrop`,
`trim`, `rotate`, `auto-orient`, `blur`, `sharpen`, `pixelate`, `bright`,
`contrast`, `circle`, `rounded-corners`, `watermark`, `quality`, `format`,
`interlace`, `info`, `average-hue`.

## Additions OSS does not have

| Feature | Notes |
|---|---|
| `format,auto` | Picks the best format the client's `Accept` header allows and this build can encode, then sends `Vary: Accept`. Highest `q` wins; `q=0` and malformed `q` are treated as "not acceptable"; `*/*` is never read as "AVIF is fine". |
| `format,jxl` | JPEG XL is not in OSS's format table. See the JPEG XL note in README. |
| `format,jpeg` | Accepted as an alias for `jpg`. OSS spells it `jpg` only. |
| `format,f_png` | The `key_value` spelling is accepted alongside OSS's bare `format,png`. |
| `x-istore-signature` | Optional HMAC request signing, off unless `ISTORE_KEY` and `ISTORE_SALT` are both set. See README. |
| `GET /healthz` | Liveness probe; exempt from signing. |
| `ISTORE_UPSTREAM` | Sources can come from another HTTP origin instead of a directory. See [Upstream mode](#upstream-mode). |

## Format conversion

| Case | OSS | iStore |
|---|---|---|
| `format,gif` on a non-GIF source | Keeps the source format | Same — verified `jpg→jpeg`, `png→png`, `gif→gif` |
| WebP output wider or taller than 16383 px | Conversion fails | Rescales proportionally and returns `200`, with a `WARN` in the log. A 16400×8 source comes out 16383×8 — a 0.1% resample, no content cropped (verified by marking both edges). |
| Transparency → **BMP** | Fills white | Writes a 32-bit BITMAPV5 preserving alpha. Most decoders — libvips included when reading back — ignore BMP alpha and render the transparent area **black**. |
| Transparency → JPEG | Fills white | Same |
| Transparency → PNG / WebP | — | Preserved |
| HEIC with alpha → `jpg` | Refused | Flattened to white and converted |
| Parameter order | Sequential; the docs require `format` last when the chain also resizes | Order-insensitive. `format,png/resize,w_100` and `resize,w_100/format,png` produce byte-identical output, because the whole chain is folded into one options bag and the output format is decided once, at save time. |
| `quality,Q_100` | Absolute quality | Supported. So is `q_` — see [Numeric approximations](#numeric-approximations). |
| `heic` / `avif` availability | Only in certain regions | Depends on the build; see [Build-dependent behaviour](#build-dependent-behaviour). |

## `info`

The eight-field shape for an image carrying resolution metadata is byte-identical
to OSS's documented example, key order included.

| Case | OSS | iStore |
|---|---|---|
| Source with **no** resolution metadata | The documented no-EXIF example returns all eight fields, `XResolution` `1/1`, `ResolutionUnit` `1` | Omits `ResolutionUnit`, `XResolution` and `YResolution`, returning five fields. **A JPEG is normally unaffected** — a JFIF `APP0` counts as resolution metadata, so an ordinary JPEG still returns eight. A PNG, WebP, GIF, TIFF or BMP with nothing to say returns five. |
| PNG EXIF | Documented as not parsed | Parsed from the `eXIf` chunk — a PNG with EXIF returns the same 25 fields a JPEG would |
| EXIF value formatting | exiv2's pretty-printer | Raw EXIF types: `FNumber` is `14/5` not `f/2.8`, `GPSLatitude` is `31/1 13/1 1236/25` not `31deg 13' 49.44"`, `ColorSpace` is `65535` |
| Tags with no standard name, and `UNDEFINED`-typed tags such as `MakerNote` | Emitted | Skipped rather than dumped as hex |
| EXIF `IFD1` (thumbnail) | Its tags appear in OSS's example | Walked, and `IFD0` wins any tag the two share — a camera writes 72 dpi in `IFD1` whatever the image's real resolution is |
| `FrameCount` for animated WebP, or a GIF longer than 64 KiB | Real count | Real count, obtained from libvips. Those two cases cost a full file read where the rest of `info` costs a 64 KiB header parse. |
| `FrameCount` for APNG | — | The real count from the `acTL` chunk, even though the pipeline cannot render those frames — see below |

> The resolution-field row is the one deliberate deviation here whose premise is
> **unconfirmed**. OSS's page states no rule either way, and its own no-EXIF
> example includes the three fields. The reasoning for omitting them is that the
> example image, being a JPEG, almost certainly carried a JFIF block. If a client
> of yours reads `XResolution` unconditionally, this is the row to revisit.

## Animation

| Case | OSS | iStore |
|---|---|---|
| Animated output formats | — | WebP and GIF only. `format,avif` flattens to the first frame, because libvips writes single-image HEIF. |
| Source longer than the frame cap | Bounded by total pixels only | `422 Source animation has N frames, limit is 300`. Refused rather than trimmed: a trimmed animation is well-formed at the wrong length and the caller cannot tell. Only animated *output* is affected — the same source asked for as JPEG is a still and is served. |
| APNG asked for as `format,webp` or `format,gif` | — | `422 This build of libvips cannot read png animation`. Any still format, a bare `resize`, and `format,auto` all flatten and are served normally. The refusal is a runtime comparison, not a version check, so it retires itself on any build that can read APNG frames. |

## Numeric approximations

None of these can be byte-identical to OSS; they are calibrated, not derived.

| Parameter | Note |
|---|---|
| `quality,q_N` | OSS's `q_` is *relative* — N percent of the source's own quality. iStore treats it as absolute. Near iStore's default the two agree closely; on a very low-quality source `q_` yields a larger file here than on OSS. `Q_` is absolute on both. |
| `sharpen`, `bright`, `contrast` | Mappings onto libvips operations, calibrated by eye against OSS output |
| `watermark` text shadow geometry | OSS specifies only the shadow's transparency; the offset and blur are scaled from the font size here |
| `watermark,type_` | Which font a name resolves to is the host's fontconfig. Install `font-wqy-zenhei` to match OSS's default face. |

## Limits and defaults

iStore is self-hosted, so these are configuration rather than platform policy.
The pixel budget matches OSS deliberately; the rest are iStore's own.

| Limit | OSS | iStore default | Environment variable |
|---|---|---|---|
| Total pixels (`width × height × frames`) | 250,000,000 | 250,000,000 | `ISTORE_MAX_SRC_RESOLUTION` |
| Single dimension | 30,000 px | unbounded | `ISTORE_MAX_RESULT_DIMENSION` |
| Single dimension for `rotate` | 4,096 px | unbounded — a 5000 px source rotates fine | — |
| Animation frames | pixel budget only | 300 | `ISTORE_MAX_ANIMATION_FRAMES` |
| Source file size | — | 100 MiB | `ISTORE_MAX_SOURCE_BYTES` |
| Watermark pixels | not documented | 8 MP | `ISTORE_MAX_WATERMARK_RESOLUTION` |
| Watermark file size | not documented | 16 MiB | `ISTORE_MAX_WATERMARK_BYTES` |
| Per-request processing time | — | 20 s | `ISTORE_PROCESS_TIMEOUT_MS` |

A watermark gets its own budgets rather than the source image's, and they are
much tighter: 8 MP against the source's 250 MP. A watermark is a decoration
composited onto an image, and the source ceiling is far too generous for one —
a 440 KB PNG that decodes to 144 MP passed it and cost 444 MB of RSS for a
single request. `watermark,image_` naming an object past either limit returns
`413 SourceTooLarge`, decided from the header before anything is decoded. Raise
the limits if a deployment genuinely composites something large.

The pixel budget is set to OSS's 250 MP rather than something smaller, so a
source OSS would accept is not refused here. What that costs depends on the
request, not on the source alone — measured on a 17000×14000 JPEG (238 MP) with
`ISTORE_CONCURRENCY=1`, peak RSS over one request:

| Request | Peak RSS |
|---|---|
| `resize,w_200` | 58 MiB — shrink-on-load, the full frame is never materialised |
| `format,jpg` | 787 MiB |
| `format,webp` | 1143 MiB |

A thumbnailing workload therefore sits near nothing, while a full-size transcode
near the ceiling costs about a gigabyte, multiplied by `ISTORE_CONCURRENCY`.
`ISTORE_MAX_SOURCE_BYTES` (100 MiB) bounds the file but not the pixel count — a
238 MP JPEG of a flat colour is under 4 MiB — so it is not a substitute for
sizing the box.

The single-dimension rows are the remaining gap: OSS refuses a source wider or
taller than 30,000 px, and 4,096 px for `rotate`; iStore enforces neither, so it
accepts shapes OSS would reject.

## Build-dependent behaviour

iStore is supported on **Linux only** — that is what CI builds and tests, and
the only platform any of this was measured on. The rows below are about the
libvips a Linux binary was linked against, not about iStore. The encodable set
is probed by a real encode at startup and logged:

```
level=INFO msg="encodable formats" formats="[jpg png webp gif avif jxl tiff bmp ico]"
```

| Case | Behaviour |
|---|---|
| A format this build cannot encode | `400 format "heic" cannot be produced by this build of libvips`, before any work starts |
| TIFF sources | The loader's `unlimited` flag needs libvips 8.17 built against libtiff 4.7+. Where it is absent libvips keeps its own decode limits; TIFFs still load. |
| Animated JPEG XL | Needs libvips 8.16 to read; older builds see a still |
| APNG | No released libvips reads APNG frames. It is on master under an unreleased `8.19.0` heading, in the libpng path only and behind `PNG_APNG_SUPPORTED`, so a future release is necessary but not sufficient. See Animation above. |

## Upstream mode

Everything above describes iStore serving a local directory, which is what all
of it was measured against. `ISTORE_UPSTREAM` fetches sources from another HTTP
origin instead. The OSS grammar, the pipeline and every response shape are
unchanged; what follows is the short list of behaviour that is not, and it is
about iStore against itself, not about OSS.

| Case | Local mode | Upstream mode |
|---|---|---|
| A path that is a directory | `404 NoSuchKey` | Whatever the origin returns. A directory-listing origin answers `200` with its HTML, and the untouched-source endpoint passes it through, because that endpoint is a pass-through by definition. Any `x-oss-process` chain on the same path still gets `422 InvalidImage`. |
| Cache invalidation | Size and mtime are in the key, so an edited file misses immediately | The origin's `ETag`, else `Last-Modified`, is in the key. With neither, the key carries a `ISTORE_UPSTREAM_TTL_SEC` time bucket (300 s), so a replaced object is picked up within one TTL rather than immediately |
| `info` on a source needing a whole-object read | Bounded by `ISTORE_MAX_SOURCE_BYTES` (100 MiB) | Bounded by `ISTORE_UPSTREAM_INFO_MAX_BYTES` (10 MiB) as well. The ranged path — every JPEG, PNG, GIF and WebP whose header answers — is unaffected and costs one 64 KiB response at any file size |
| `info` on an origin that ignores `Range` | — | Still one request, still 64 KiB: the `200` body is capped and closed rather than downloaded |
| Origin returns 4xx or 5xx | — | Passed through with the origin's own status and `{"Code":"UpstreamError"}`. `404` and `410` become the usual `404 NoSuchKey` |
| Origin returns 3xx | — | `502`. Redirects are not followed — doing so would hand the choice of destination back to the origin, and passing the 3xx to the client would send it to fetch the unprocessed original |
| Origin unreachable / times out | — | `502` / `504`. The fetch has its own budget, `ISTORE_UPSTREAM_TIMEOUT_MS` (10 s), separate from `ISTORE_PROCESS_TIMEOUT_MS` |
| Client `Range` requests | Not supported | Not supported. iStore's own request to the origin is ranged; a client asking iStore for a range still gets the whole object |
| Watermark objects | Read from the root, cached in memory for the process's life | Fetched from the origin, cached for `ISTORE_UPSTREAM_TTL_SEC`. Either way the cache holds at most 64 watermarks and `ISTORE_WATERMARK_CACHE_BYTES` of them, least-recently-used first out, and concurrent first-time requests for one watermark are collapsed into a single fetch |
| A `HEAD` of the untouched source | Opens the file, reads nothing | One ranged request for 512 bytes — enough to sniff the format; the length comes from `Content-Range` |
| Origin stalls or hangs up mid-object | — | `504` / `502`, the same as a failure on the response headers. A `GET` already streaming when that happens ends as a truncated response, which is what any proxy does once the headers are out |

The error bodies never name the origin or repeat what it said. Which internal
host iStore talks to is not the client's business; the operator gets the URL and
the origin's status in the log.

## Errors

The envelope matches OSS's shape — `{"Code": "...", "Message": "..."}` with the
same JSON layout — so a client written against OSS parses iStore's failures. The
`Code` strings themselves have **not** been cross-checked against OSS's list;
iStore emits `NoSuchKey`, `InvalidArgument`, `InvalidImage`, `SourceTooLarge`,
`AccessDenied`, `MethodNotAllowed`, `RequestTimeout`, `InternalError` and — in
upstream mode only — `UpstreamError`.

A path that escapes the root, names a directory, or does not exist all return the
same `404 NoSuchKey`, deliberately: distinguishing them maps the filesystem for
whoever is probing.

---

## Verified to match

Checked against the OSS documentation and found consistent, listed so a reader
knows they were looked at rather than overlooked:

- the eight `format` values `jpg` `png` `webp` `bmp` `gif` `tiff` `heic` `avif`
- `format,gif` keeping a non-GIF source in its own format
- transparency filling white on JPEG output
- `quality,Q_100`
- the `info` field shape, key order, `{"value": "..."}` wrapping, all-string
  values, and `jpg` rather than `jpeg` in `Format`
- `average-hue` returning bare `0xRRGGBB` as plain text, not JSON
- `info` and `average-hue` being terminal — neither can be chained with a
  transform

## Sources

- [对图片进行格式转换](https://help.aliyun.com/zh/oss/user-guide/convert-image-formats-2)
- [查询图片的EXIF信息](https://help.aliyun.com/zh/oss/user-guide/query-the-exif-data-of-an-image-4)
- [OSS图片处理常见问题及处理方法](https://help.aliyun.com/zh/oss/user-guide/faq-2)
