#include "vips.h"
#include <string.h>

#define VIPS_SCRGB_ALPHA_FIXED \
  (VIPS_MAJOR_VERSION > 8 || (VIPS_MAJOR_VERSION == 8 && VIPS_MINOR_VERSION >= 15))

// jxlload gained animation support — and with it the `page` and `n` load
// options — in libvips 8.16. Passing them to an older loader is not ignored:
// vips_jxlload_source fails outright with "no property named `page'", so every
// JXL *source* is unreadable on 8.15 even though JXL output works fine.
#define VIPS_JXL_HAS_PAGES \
  (VIPS_MAJOR_VERSION > 8 || (VIPS_MAJOR_VERSION == 8 && VIPS_MINOR_VERSION >= 16))

#define VIPS_META_PALETTE_BITS_DEPTH "palette-bit-depth"

#define IMGPROXY_META_ICC_NAME "imgproxy-icc-profile"
#define IMGPROXY_ICC_IMPORTED "imgproxy-icc-imported"

int
vips_initialize()
{
  extern GType vips_foreign_load_bmp_source_get_type(void);
  vips_foreign_load_bmp_source_get_type();

  extern GType vips_foreign_load_bmp_buffer_get_type(void);
  vips_foreign_load_bmp_buffer_get_type();

  extern GType vips_foreign_save_bmp_target_get_type(void);
  vips_foreign_save_bmp_target_get_type();

  extern GType vips_foreign_load_ico_source_get_type(void);
  vips_foreign_load_ico_source_get_type();

  extern GType vips_foreign_load_ico_buffer_get_type(void);
  vips_foreign_load_ico_buffer_get_type();

  extern GType vips_foreign_save_ico_target_get_type(void);
  vips_foreign_save_ico_target_get_type();

  return vips_init("imgproxy");
}

void
unref_image(VipsImage *in)
{
  VIPS_UNREF(in);
}

void
g_free_go(void **buf)
{
  g_free(*buf);
}

int
gif_resolution_limit()
{
  return INT_MAX / 4;
}

// Just create and destroy a tiny image to ensure vips is operational
int
vips_health()
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 2);

  int res = vips_black(&t[0], 4, 4, "bands", 4, NULL) ||
      !(t[1] = vips_image_copy_memory(t[0]));

  VIPS_UNREF(base);

  return res;
}

int
check_shrink(const char *function, double shrink)
{
  if (shrink != 0)
    return 0;

  vips_error(function, "shrink can't be 0");
  return -1;
}

// loads jpeg from a source
int
vips_jpegload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  return check_shrink("vips_jpegload_source_go", lo.Shrink) ||
      vips_jpegload_source(
          VIPS_SOURCE(source), out,
          "access", VIPS_ACCESS_SEQUENTIAL,
          "shrink", (int) lo.Shrink,
          NULL);
}

// loads jxl from source
int
vips_jxlload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
#if VIPS_JXL_HAS_PAGES
  return vips_jxlload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      "page", lo.Page,
      "n", lo.Pages,
      NULL);
#else
  // Pre-8.16 this loader has no concept of pages, so it always yields the first
  // frame. That is consistent rather than wrong: without page metadata
  // Image.IsAnimated() stays false, so the animated path is never entered and
  // nothing downstream expects frames that are not there.
  (void) lo;

  return vips_jxlload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      NULL);
#endif
}

int
vips_pngload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  return vips_pngload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      "unlimited", lo.PngUnlimited,
      NULL);
}

int
vips_webpload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  return check_shrink("vips_webpload_source_go", lo.Shrink) ||
      vips_webpload_source(
          VIPS_SOURCE(source), out,
          "access", VIPS_ACCESS_SEQUENTIAL,
          "scale", 1.0 / lo.Shrink,
          "page", lo.Page,
          "n", lo.Pages,
          NULL);
}

int
vips_gifload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  return vips_gifload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      "page", lo.Page,
      "n", lo.Pages,
      NULL);
}

int
vips_svgload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  if (check_shrink("vips_svgload_source_go", lo.Shrink))
    return -1;

  double scale = 1.0 / lo.Shrink;

  /* libvips uses default DPI of 72, but W3C recommends 96.
   */
  double dpi = 96.0;
  /* Adjust the scale to account for the difference in DPI so that the output size is correct.
   */
  scale = scale * 72.0 / dpi;

  return vips_svgload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      "scale", scale,
      "dpi", dpi,
      "unlimited", lo.SvgUnlimited,
      NULL);
}

int
vips_heifload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  return vips_heifload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      "thumbnail", lo.Thumbnail,
      NULL);
}

int
vips_tiffload_source_go(VipsImgproxySource *source, VipsImage **out, ImgproxyLoadOptions lo)
{
  return vips_tiffload_source(
      VIPS_SOURCE(source), out,
      "access", VIPS_ACCESS_SEQUENTIAL,
      "page", lo.Page,
      "n", lo.Pages,
      "unlimited", lo.TiffUnlimited,
      NULL);
}

int
vips_black_go(VipsImage **out, int width, int height, int bands)
{
  VipsImage *tmp = NULL;

  int res = vips_black(&tmp, width, height, "bands", bands, NULL) ||
      vips_copy(tmp, out, "interpretation", VIPS_INTERPRETATION_sRGB, NULL);

  VIPS_UNREF(tmp);

  return res;
}

int
vips_fix_scRGB_alpha_tiff(VipsImage *in, VipsImage **out)
{
#if VIPS_SCRGB_ALPHA_FIXED
  /* Vips 8.15+ uses 0.0-1.0 range for linear alpha, so we don't need a fix.
   */
  return vips_copy(in, out, NULL);
#else
  /* Vips prior to 8.14 loads linear alpha in the 0.0-1.0 range but uses the 0.0-255.0 range.
   */
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 4);

  int res =
      vips_extract_band(in, &t[0], 0, "n", 3, NULL) ||
      vips_extract_band(in, &t[1], 3, "n", in->Bands - 3, NULL) ||
      vips_linear1(t[1], &t[2], 255.0, 0, NULL) ||
      vips_cast(t[2], &t[3], in->BandFmt, NULL) ||
      vips_bandjoin2(t[0], t[3], out, NULL);

  VIPS_UNREF(base);

  return res;
#endif
}

/* Vips loads linear BW TIFFs as VIPS_INTERPRETATION_B_W or VIPS_INTERPRETATION_GREY16
 * but these colourspaces are not linear. We should properly convert them to
 * VIPS_INTERPRETATION_GREY16
 */
int
vips_fix_BW_float_tiff(VipsImage *in, VipsImage **out)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 8);

  VipsImage *color = in;
  VipsImage *alpha = NULL;

  /* Extract and fix alpha. Float WB TIFF uses the 0.0-1.0 range but we need
   * the 0.0-65535.0 range
   */
  if (in->Bands > 1) {
    if (
        vips_extract_band(in, &t[0], 0, NULL) ||
        vips_extract_band(in, &t[1], 1, "n", in->Bands - 1, NULL) ||
        vips_linear1(t[1], &t[2], 65535.0, 0, NULL) ||
        vips_cast_ushort(t[2], &t[3], NULL) ||
        vips_copy(t[3], &t[4], "interpretation", VIPS_INTERPRETATION_GREY16, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    color = t[0];
    alpha = t[4];
  }

  /* Craft an scRGB image and convert it back to GREY16 to apply a gamma
   * correction
   */
  VipsImage *rgb[3] = { color, color, color };
  if (
      vips_bandjoin(rgb, &t[5], 3, NULL) ||
      vips_colourspace(t[5], &t[6], VIPS_INTERPRETATION_GREY16,
          "source_space", VIPS_INTERPRETATION_scRGB, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  int res;

  if (alpha)
    res =
        vips_bandjoin2(t[6], alpha, &t[7], NULL) ||
        vips_icc_remove(t[7], out);
  else
    res = vips_icc_remove(t[6], out);

  VIPS_UNREF(base);

  return res;
}

int
vips_fix_float_tiff(VipsImage *in, VipsImage **out)
{
  /* Vips loads linear alpha in the 0.0-1.0 range but uses the 0.0-255.0 range.
   * https://github.com/libvips/libvips/pull/3627 fixes this behavior
   */
  if (in->Type == VIPS_INTERPRETATION_scRGB && in->Bands > 3)
    return vips_fix_scRGB_alpha_tiff(in, out);

  /* Vips loads linear BW TIFFs as VIPS_INTERPRETATION_B_W or VIPS_INTERPRETATION_GREY16
   * but these colourspaces are not linear. We should properly convert them to
   * VIPS_INTERPRETATION_GREY16
   */
  if (
      (in->Type == VIPS_INTERPRETATION_B_W || in->Type == VIPS_INTERPRETATION_GREY16) &&
      (in->BandFmt == VIPS_FORMAT_FLOAT || in->BandFmt == VIPS_FORMAT_DOUBLE))
    return vips_fix_BW_float_tiff(in, out);

  return vips_copy(in, out, NULL);
}

int
vips_get_orientation(VipsImage *image)
{
  int orientation;

  if (
      vips_image_get_typeof(image, VIPS_META_ORIENTATION) == G_TYPE_INT &&
      vips_image_get_int(image, VIPS_META_ORIENTATION, &orientation) == 0)
    return orientation;

  return 1;
}

int
vips_get_palette_bit_depth(VipsImage *image)
{
  int palette, palette_bit_depth;

#ifdef VIPS_META_PALETTE
  if (vips_image_get_typeof(image, VIPS_META_PALETTE) == G_TYPE_INT &&
      vips_image_get_int(image, VIPS_META_PALETTE, &palette) == 0 &&
      palette) {

    if (vips_image_get_typeof(image, VIPS_META_BITS_PER_SAMPLE) == G_TYPE_INT &&
        vips_image_get_int(image, VIPS_META_BITS_PER_SAMPLE, &palette_bit_depth) == 0)
      return palette_bit_depth;

    else
      /* Image has palette but VIPS_META_BITS_PER_SAMPLE is not set.
       * It's very unlikely but we should handle this
       */
      return 8;
  }
#else
  if (vips_image_get_typeof(image, VIPS_META_PALETTE_BITS_DEPTH) == G_TYPE_INT &&
      vips_image_get_int(image, VIPS_META_PALETTE_BITS_DEPTH, &palette_bit_depth) == 0)
    return palette_bit_depth;
#endif

  return 0;
}

VipsBandFormat
vips_band_format(VipsImage *in)
{
  return in->BandFmt;
}

gboolean
vips_image_is_animated(VipsImage *in)
{
  int n_pages;

  return (vips_image_get_typeof(in, "delay") != G_TYPE_INVALID &&
      vips_image_get_typeof(in, "loop") != G_TYPE_INVALID &&
      vips_image_get_typeof(in, "n-pages") == G_TYPE_INT &&
      vips_image_get_int(in, "n-pages", &n_pages) == 0 &&
      n_pages > 1);
}

int
vips_image_remove_animation(VipsImage *in, VipsImage **out)
{
  if (vips_copy(in, out, NULL))
    return -1;

  vips_image_remove(*out, "delay");
  vips_image_remove(*out, "loop");
  vips_image_remove(*out, "page-height");
  vips_image_remove(*out, "n-pages");

  return 0;
}

int
vips_image_get_array_int_go(VipsImage *image, const char *name, int **out, int *n)
{
  return vips_image_get_array_int(image, name, out, n);
}

void
vips_image_set_array_int_go(VipsImage *image, const char *name, const int *array, int n)
{
  vips_image_set_array_int(image, name, array, n);
}

int
vips_addalpha_go(VipsImage *in, VipsImage **out)
{
  return vips_addalpha(in, out, NULL);
}

int
vips_copy_go(VipsImage *in, VipsImage **out)
{
  return vips_copy(in, out, NULL);
}

int
vips_cast_go(VipsImage *in, VipsImage **out, VipsBandFormat format)
{
  return vips_cast(in, out, format, NULL);
}

int
vips_rad2float_go(VipsImage *in, VipsImage **out)
{
  return vips_rad2float(in, out, NULL);
}

int
vips_resize_go(VipsImage *in, VipsImage **out, double wscale, double hscale)
{
  if (!vips_image_hasalpha(in))
    return vips_resize(in, out, wscale, "vscale", hscale, NULL);

  VipsBandFormat format = vips_band_format(in);

  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 4);

  int res =
      vips_premultiply(in, &t[0], NULL) ||
      vips_cast(t[0], &t[1], format, NULL) ||
      vips_resize(t[1], &t[2], wscale, "vscale", hscale, NULL) ||
      vips_unpremultiply(t[2], &t[3], NULL) ||
      vips_cast(t[3], out, format, NULL);

  VIPS_UNREF(base);

  return res;
}

/* We don't really need to return the size since we check if the buffer is at least
 * the size of ICC header, and all we need is a header
 */
static const void *
vips_icc_get_header(VipsImage *in)
{
  const void *data = NULL;
  size_t data_len = 0;

  if (!vips_image_get_typeof(in, VIPS_META_ICC_NAME) ||
      vips_image_get_blob(in, VIPS_META_ICC_NAME, &data, &data_len))
    return NULL;

  /* Less than header size
   */
  if (!data || data_len < 128)
    return NULL;

  return data;
}

int
vips_icc_is_srgb_iec61966(VipsImage *in)
{
  // 1998-12-01
  static char date[] = { 7, 206, 0, 2, 0, 9 };
  // 2.1
  static char version[] = { 2, 16, 0, 0 };

  const void *data = vips_icc_get_header(in);
  if (!data)
    return FALSE;

  /* Predict it is sRGB IEC61966 2.1 by checking some header fields
   */
  return ((memcmp(data + 48, "IEC ", 4) == 0) && // Device manufacturer
      (memcmp(data + 16, "RGB ", 4) == 0) &&     // Colorspace
      (memcmp(data + 52, "sRGB", 4) == 0) &&     // Device model
      (memcmp(data + 80, "HP  ", 4) == 0) &&     // Profile creator
      (memcmp(data + 24, date, 6) == 0) &&       // Date of creation
      (memcmp(data + 8, version, 4) == 0));      // Version
}

static VipsPCS
vips_icc_get_pcs(VipsImage *in)
{
  const void *data = vips_icc_get_header(in);
  if (!data)
    return VIPS_PCS_LAB;

  if (memcmp(data + 20, "XYZ ", 4) == 0)
    return VIPS_PCS_XYZ;

  return VIPS_PCS_LAB;
}

int
vips_has_embedded_icc(VipsImage *in)
{
  return vips_image_get_typeof(in, VIPS_META_ICC_NAME) != 0;
}

int
vips_icc_backup(VipsImage *in, VipsImage **out)
{
  if (vips_copy(in, out, NULL))
    return 1;

  if (!vips_image_get_typeof(in, VIPS_META_ICC_NAME))
    return 0;

  const void *data = NULL;
  size_t data_len = 0;

  if (vips_image_get_blob(in, VIPS_META_ICC_NAME, &data, &data_len))
    return 0;

  if (!data || data_len < 128)
    return 0;

  vips_image_remove(*out, IMGPROXY_META_ICC_NAME);
  vips_image_set_blob_copy(*out, IMGPROXY_META_ICC_NAME, data, data_len);

  return 0;
}

int
vips_icc_restore(VipsImage *in, VipsImage **out)
{
  if (vips_copy(in, out, NULL))
    return 1;

  if (vips_image_get_typeof(in, VIPS_META_ICC_NAME) ||
      !vips_image_get_typeof(in, IMGPROXY_META_ICC_NAME))
    return 0;

  const void *data = NULL;
  size_t data_len = 0;

  if (vips_image_get_blob(in, IMGPROXY_META_ICC_NAME, &data, &data_len))
    return 0;

  if (!data || data_len < 128)
    return 0;

  vips_image_remove(*out, VIPS_META_ICC_NAME);
  vips_image_set_blob_copy(*out, VIPS_META_ICC_NAME, data, data_len);

  return 0;
}

int
vips_icc_import_go(VipsImage *in, VipsImage **out)
{
  if (vips_image_get_typeof(in, IMGPROXY_ICC_IMPORTED) != 0)
    return vips_copy(in, out, NULL);

  /* Skip import if the image has coded pixels
   */
  if (in->Coding != VIPS_CODING_NONE)
    return vips_copy(in, out, NULL);

  /* Only import profiles for 8 and 16 bit images
   */
  if (in->BandFmt != VIPS_FORMAT_UCHAR && in->BandFmt != VIPS_FORMAT_USHORT)
    return vips_copy(in, out, NULL);

  /* No profile – nothing to import
   */
  if (!vips_has_embedded_icc(in))
    return vips_copy(in, out, NULL);

  /* Skip importing sRGB IEC61966 2.1 profile since it's basically the same as
   * no profile, so we can avoid an expensive color conversion
   */
  if (vips_image_guess_interpretation(in) == VIPS_INTERPRETATION_sRGB &&
      vips_icc_is_srgb_iec61966(in))
    return vips_copy(in, out, NULL);

  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 5);

  int has_alpha_16 = FALSE;

  /* RGB16 and GREY16 images have max alpha 65535, but this is not handled by
   * vips_icc_import. We need to extract the alpha channel and convert it to 0-255
   */
  if ((in->Type == VIPS_INTERPRETATION_RGB16 && in->Bands > 3) ||
      (in->Type == VIPS_INTERPRETATION_GREY16 && in->Bands > 1)) {
    int bands = in->Type == VIPS_INTERPRETATION_RGB16 ? 3 : 1;

    if (vips_extract_band(in, &t[0], 0, "n", bands, NULL) ||
        vips_extract_band(in, &t[1], bands, "n", 1, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    in = t[0];
    has_alpha_16 = TRUE;
  }

  if (vips_icc_import(in, out, "embedded", TRUE, "pcs", vips_icc_get_pcs(in), NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  /* Convert 16-bit alpha channel to 0-255 range and join it back to the image
   */
  if (has_alpha_16) {
    t[2] = *out;
    *out = NULL;

    if (vips_cast(t[1], &t[3], t[2]->BandFmt, NULL) ||
        vips_linear1(t[3], &t[4], 1.0 / 255.0, 0, NULL) ||
        vips_bandjoin2(t[2], t[4], out, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }
  }

  vips_image_set_int(*out, IMGPROXY_ICC_IMPORTED, 1);

  VIPS_UNREF(base);

  return 0;
}

int
image_depth(VipsImage *in)
{
  switch (in->Type) {
  case VIPS_INTERPRETATION_GREY16:
  case VIPS_INTERPRETATION_RGB16:
  case VIPS_INTERPRETATION_scRGB:
    return 16;
  default:
    return 8;
  }
}

// vips_guard_colorspace ensures that the image is in a suitable colorspace
// (sRGB, RGB16, B_W or GREY16) we know of and can process. If mustOutputRGB
// is true, a grayscale image will be converted to colorful version.
int
vips_guard_colorspace(VipsImage *in, VipsImage **out, gboolean mustOutputRGB)
{
  VipsInterpretation interp = vips_image_guess_interpretation(in);
  VipsInterpretation out_interp = interp;
  VipsBandFormat fmt;

  switch (interp) {
  case VIPS_INTERPRETATION_B_W:
    if (mustOutputRGB) {
      out_interp = VIPS_INTERPRETATION_sRGB;
    }
    break; // otherwise keep B_W

  case VIPS_INTERPRETATION_GREY16:
    if (mustOutputRGB) {
      out_interp = VIPS_INTERPRETATION_RGB16;
    }
    break; // otherwise keep GREY16

  // formally, this could be handled by default case, but
  // the probability of scRGB is high enough so let's save
  // vips_image_get_format call
  case VIPS_INTERPRETATION_scRGB:
    out_interp = VIPS_INTERPRETATION_RGB16;
    break;

  // keep as is
  case VIPS_INTERPRETATION_sRGB:
  case VIPS_INTERPRETATION_RGB16:
    break;

  // 8 bit anything becomes sRGB, 16+ bit anything becomes RGB16
  default:
    fmt = vips_image_get_format(in);

    if ((fmt == VIPS_FORMAT_UCHAR) || (fmt == VIPS_FORMAT_CHAR)) {
      out_interp = VIPS_INTERPRETATION_sRGB;
    }
    else {
      out_interp = VIPS_INTERPRETATION_RGB16;
    }
    break;
  }

  /* If the image is already in the desired colorspace, just copy it
   */
  if (out_interp == interp)
    return vips_copy(in, out, NULL);

  VipsImage *tmp = NULL;

  int res = vips_icc_import_go(in, &tmp) ||
      vips_colourspace(tmp, out, out_interp, NULL);

  VIPS_UNREF(tmp);

  return res;
}

int
vips_icc_export_go(VipsImage *in, VipsImage **out)
{
  /* No profile – nothing to export
   */
  if (!vips_has_embedded_icc(in))
    return vips_copy(in, out, NULL);

  /* Skip exporting sRGB IEC61966 2.1 profile since it's basically the same as
   * no profile, so we can avoid an expensive color conversion
   */
  if (vips_image_guess_interpretation(in) == VIPS_INTERPRETATION_sRGB &&
      vips_icc_is_srgb_iec61966(in))
    return vips_copy(in, out, NULL);

  return vips_icc_export(
      in, out,
      "pcs", vips_icc_get_pcs(in),
      "depth", image_depth(in),
      NULL);
}

int
vips_icc_transform_standard(VipsImage *in, VipsImage **out)
{
  /* No profile – nothing to transform
   */
  if (!vips_has_embedded_icc(in))
    return vips_copy(in, out, NULL);

  /* Skip transforming sRGB IEC61966 2.1 profile since it's basically the same as
   * the standard profile, so we can avoid an expensive color conversion
   */
  if (vips_image_guess_interpretation(in) == VIPS_INTERPRETATION_sRGB &&
      vips_icc_is_srgb_iec61966(in))
    return vips_copy(in, out, NULL);

  const char *profile =
      (in->Type == VIPS_INTERPRETATION_B_W || in->Type == VIPS_INTERPRETATION_GREY16)
      ? "sGrey"
      : "sRGB";

  return vips_icc_transform(
      in, out,
      profile,
      "embedded", TRUE,
      "pcs", vips_icc_get_pcs(in),
      "depth", image_depth(in),
      NULL);
}

int
vips_icc_remove(VipsImage *in, VipsImage **out)
{
  if (vips_copy(in, out, NULL))
    return 1;

  vips_image_remove(*out, VIPS_META_ICC_NAME);
  vips_image_remove(*out, IMGPROXY_META_ICC_NAME);
  vips_image_remove(*out, "exif-ifd0-WhitePoint");
  vips_image_remove(*out, "exif-ifd0-PrimaryChromaticities");
  vips_image_remove(*out, "exif-ifd2-ColorSpace");

  return 0;
}

int
vips_colourspace_go(VipsImage *in, VipsImage **out, VipsInterpretation cs)
{
  return vips_colourspace(in, out, cs, NULL);
}

int
vips_rot_go(VipsImage *in, VipsImage **out, VipsAngle angle)
{
  return vips_rot(in, out, angle, NULL);
}

int
vips_flip_horizontal_go(VipsImage *in, VipsImage **out)
{
  return vips_flip(in, out, VIPS_DIRECTION_HORIZONTAL, NULL);
}

int
vips_flip_vertical_go(VipsImage *in, VipsImage **out)
{
  return vips_flip(in, out, VIPS_DIRECTION_VERTICAL, NULL);
}

int
vips_smartcrop_go(VipsImage *in, VipsImage **out, int width, int height)
{
  return vips_smartcrop(in, out, width, height, NULL);
}

int
vips_apply_filters(VipsImage *in, VipsImage **out, double blur_sigma,
    double sharp_sigma, int pixelate_pixels)
{

  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 10);

  VipsInterpretation interpretation = in->Type;
  VipsBandFormat format = in->BandFmt;
  gboolean premultiplied = FALSE;

  if ((blur_sigma > 0 || sharp_sigma > 0) && vips_image_hasalpha(in)) {
    if (
        vips_premultiply(in, &t[0], NULL) ||
        vips_cast(t[0], &t[1], format, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    in = t[1];
    premultiplied = TRUE;
  }

  if (blur_sigma > 0.0) {
    if (vips_gaussblur(in, &t[2], blur_sigma, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    in = t[2];
  }

  if (sharp_sigma > 0.0) {
    if (vips_sharpen(in, &t[3], "sigma", sharp_sigma, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    in = t[3];
  }

  pixelate_pixels = VIPS_MIN(pixelate_pixels, VIPS_MAX(in->Xsize, in->Ysize));

  if (pixelate_pixels > 1) {
    int w, h, tw, th;

    w = in->Xsize;
    h = in->Ysize;

    tw = (int) ceil((double) w / pixelate_pixels) * pixelate_pixels;
    th = (int) ceil((double) h / pixelate_pixels) * pixelate_pixels;

    if (tw > w || th > h) {
      if (vips_embed(in, &t[4], 0, 0, tw, th, "extend", VIPS_EXTEND_MIRROR, NULL)) {
        VIPS_UNREF(base);
        return 1;
      }

      in = t[4];
    }

    if (
        vips_shrink(in, &t[5], pixelate_pixels, pixelate_pixels, NULL) ||
        vips_zoom(t[5], &t[6], pixelate_pixels, pixelate_pixels, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    in = t[6];

    if (tw > w || th > h) {
      if (vips_extract_area(in, &t[7], 0, 0, w, h, NULL)) {
        VIPS_UNREF(base);
        return 1;
      }

      in = t[7];
    }
  }

  if (premultiplied) {
    if (vips_unpremultiply(in, &t[8], NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    in = t[8];
  }

  int res =
      vips_colourspace(in, &t[9], interpretation, NULL) ||
      vips_cast(t[9], out, format, NULL);

  VIPS_UNREF(base);

  return res;
}

int
vips_flatten_go(VipsImage *in, VipsImage **out, RGB bg)
{
  if (!vips_image_hasalpha(in))
    return vips_copy(in, out, NULL);

  // When color is specified and it is not gray, we need
  // to convert the image to RGB first.
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 2);

  gboolean isBWColor = (bg.r == bg.g && bg.r == bg.b);

  if (vips_guard_colorspace(in, &t[0], !isBWColor)) {
    VIPS_UNREF(base);
    return 1;
  }

  in = t[0];

  // If the image is 16-bit, scale the background color accordingly
  if (image_depth(in) == 16) {
    bg.r = bg.r * 257.0;
    bg.g = bg.g * 257.0;
    bg.b = bg.b * 257.0;
  }

  VipsArrayDouble *bga = vips_array_double_newv(3, bg.r, bg.g, bg.b);
  int res = vips_flatten(in, out, "background", bga, NULL);
  vips_area_unref((VipsArea *) bga);

  VIPS_UNREF(base);

  return res;
}

int
vips_extract_area_go(VipsImage *in, VipsImage **out, int left, int top, int width, int height)
{
  return vips_extract_area(in, out, left, top, width, height, NULL);
}

int
vips_trim(VipsImage *in, VipsImage **out, double threshold,
    gboolean smart, RGB bg, gboolean equal_hor, gboolean equal_ver)
{

  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 2);

  VipsImage *tmp = in;

  if (vips_image_guess_interpretation(in) != VIPS_INTERPRETATION_sRGB) {
    if (vips_colourspace(in, &t[0], VIPS_INTERPRETATION_sRGB, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }
    tmp = t[0];
  }

  if (vips_image_hasalpha(tmp)) {
    RGB f_bg = { 255.0, 0, 255.0 };
    if (vips_flatten_go(tmp, &t[1], f_bg)) {
      VIPS_UNREF(base);
      return 1;
    }
    tmp = t[1];
  }

  double *img_bg = NULL;
  int img_bgn;
  VipsArrayDouble *bga;

  if (smart) {
    if (vips_getpoint(tmp, &img_bg, &img_bgn, 0, 0, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }
    bga = vips_array_double_new(img_bg, img_bgn);
  }
  else {
    bga = vips_array_double_newv(3, bg.r, bg.g, bg.b);
  }

  int left, right, top, bot, width, height, diff;
  int res = vips_find_trim(tmp, &left, &top, &width, &height, "background", bga, "threshold", threshold, NULL);

  VIPS_UNREF(base);
  vips_area_unref((VipsArea *) bga);
  g_free(img_bg);

  if (res) {
    return 1;
  }

  if (equal_hor) {
    right = in->Xsize - left - width;
    diff = right - left;
    if (diff > 0) {
      width += diff;
    }
    else if (diff < 0) {
      left = right;
      width -= diff;
    }
  }

  if (equal_ver) {
    bot = in->Ysize - top - height;
    diff = bot - top;
    if (diff > 0) {
      height += diff;
    }
    else if (diff < 0) {
      top = bot;
      height -= diff;
    }
  }

  if (width == 0 || height == 0) {
    return vips_copy(in, out, NULL);
  }

  return vips_extract_area(in, out, left, top, width, height, NULL);
}

int
vips_replicate_go(VipsImage *in, VipsImage **out, int width, int height, int centered)
{
  VipsImage *tmp = NULL;

  int across = ceil((double) width / in->Xsize);
  int down = ceil((double) height / in->Ysize);

  if (centered) {
    if (across % 2 == 0)
      across++;
    if (down % 2 == 0)
      down++;
  }

  if (vips_replicate(in, &tmp, across, down, NULL))
    return 1;

  const int left = centered ? (tmp->Xsize - width) / 2 : 0;
  const int top = centered ? (tmp->Ysize - height) / 2 : 0;

  if (vips_extract_area(tmp, out, left, top, width, height, NULL)) {
    VIPS_UNREF(tmp);
    return 1;
  }

  VIPS_UNREF(tmp);

  return 0;
}

int
vips_embed_go(VipsImage *in, VipsImage **out, int x, int y, int width, int height)
{
  VipsImage *tmp = NULL;

  if (!vips_image_hasalpha(in)) {
    if (vips_addalpha(in, &tmp, NULL))
      return 1;

    in = tmp;
  }

  int ret =
      vips_embed(in, out, x, y, width, height, "extend", VIPS_EXTEND_BLACK, NULL);

  VIPS_UNREF(tmp);

  return ret;
}

int
vips_apply_watermark(VipsImage *in, VipsImage *watermark, VipsImage **out, int left, int top, double opacity)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 7);

  if (!vips_image_hasalpha(watermark)) {
    if (vips_addalpha(watermark, &t[0], NULL))
      return 1;

    watermark = t[0];
  }

  if (opacity < 1) {
    if (
        vips_extract_band(watermark, &t[1], 0, "n", watermark->Bands - 1, NULL) ||
        vips_extract_band(watermark, &t[2], watermark->Bands - 1, "n", 1, NULL) ||
        vips_linear1(t[2], &t[3], opacity, 0, NULL) ||
        vips_bandjoin2(t[1], t[3], &t[4], NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    watermark = t[4];
  }

  int had_alpha = vips_image_hasalpha(in);

  VipsInterpretation cs = in->Type;

  // Image is black and white but watermark is not: we need to convert image to colored
  if ((cs == VIPS_INTERPRETATION_B_W || cs == VIPS_INTERPRETATION_GREY16) &&
      (watermark->Type != VIPS_INTERPRETATION_B_W && watermark->Type != VIPS_INTERPRETATION_GREY16)) {

    cs = (cs == VIPS_INTERPRETATION_B_W) ? VIPS_INTERPRETATION_sRGB : VIPS_INTERPRETATION_RGB16;
  }

  if (
      vips_composite2(
          in, watermark, &t[5], VIPS_BLEND_MODE_OVER,
          "x", left, "y", top, "compositing_space", cs,
          NULL) ||
      vips_cast(t[5], &t[6], vips_image_get_format(in), NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  int res;

  if (!had_alpha && vips_image_hasalpha(t[6])) {
    res = vips_extract_band(t[6], out, 0, "n", t[6]->Bands - 1, NULL);
  }
  else {
    res = vips_copy(t[6], out, NULL);
  }

  VIPS_UNREF(base);

  return res;
}

int
vips_linecache_seq(VipsImage *in, VipsImage **out, int tile_height)
{
  return vips_linecache(in, out, "tile_height", tile_height, "access", VIPS_ACCESS_SEQUENTIAL,
      NULL);
}

int
vips_arrayjoin_go(VipsImage **in, VipsImage **out, int n)
{
  return vips_arrayjoin(in, out, n, "across", 1, NULL);
}

typedef struct {
  int strip_all;
  int keep_exif_copyright;
  int keep_animation;
} VipsStripOptions;

void *
vips_strip_fn(VipsImage *in, const char *name, GValue *value, void *a)
{
  VipsStripOptions *opts = (VipsStripOptions *) a;

  if (strcmp(name, "vips-sequential") == 0)
    return NULL;

  if (!opts->strip_all) {
    if ((strcmp(name, VIPS_META_ICC_NAME) == 0) ||
#ifdef VIPS_META_BITS_PER_SAMPLE
        (strcmp(name, VIPS_META_BITS_PER_SAMPLE) == 0) ||
#endif
#ifdef VIPS_META_PALETTE
        (strcmp(name, VIPS_META_PALETTE) == 0) ||
#endif
        (strcmp(name, VIPS_META_PALETTE_BITS_DEPTH) == 0) ||
        (strcmp(name, "background") == 0) ||
        (strcmp(name, "vips-loader") == 0) ||
        (vips_isprefix("imgproxy-", name)))
      return NULL;

    if (opts->keep_exif_copyright)
      if ((strcmp(name, VIPS_META_EXIF_NAME) == 0) ||
          (strcmp(name, "exif-ifd0-Copyright") == 0) ||
          (strcmp(name, "exif-ifd0-Artist") == 0))
        return NULL;

    if (opts->keep_animation)
      if ((strcmp(name, "page-height") == 0) ||
          (strcmp(name, "delay") == 0) ||
          (strcmp(name, "loop") == 0) ||
          (strcmp(name, "n-pages") == 0))
        return NULL;
  }

  vips_image_remove(in, name);

  return NULL;
}

int
vips_strip(VipsImage *in, VipsImage **out, int keep_exif_copyright)
{
  static double default_resolution = 72.0 / 25.4;

  VipsStripOptions opts = {
    .strip_all = 0,
    .keep_exif_copyright = keep_exif_copyright,
    .keep_animation = FALSE,
  };

  if (vips_image_get_typeof(in, "imgproxy-is-animated") &&
      vips_image_get_int(in, "imgproxy-is-animated", &opts.keep_animation))
    opts.keep_animation = FALSE;

  if (vips_copy(
          in, out,
          "xres", default_resolution,
          "yres", default_resolution,
          NULL))
    return 1;

  vips_image_map(*out, vips_strip_fn, &opts);

  return 0;
}

int
vips_strip_all(VipsImage *in, VipsImage **out)
{
  VipsStripOptions opts = {
    .strip_all = TRUE,
    .keep_exif_copyright = FALSE,
    .keep_animation = FALSE,
  };

  if (vips_copy(in, out, NULL))
    return 1;

  vips_image_map(*out, vips_strip_fn, &opts);

  /* vips doesn't include "palette-bit-depth" to the map of fields
   */
  vips_image_remove(*out, VIPS_META_PALETTE_BITS_DEPTH);

  return 0;
}

int
vips_jpegsave_go(VipsImage *in, VipsTarget *target, int quality, ImgproxySaveOptions opts)
{
  return vips_jpegsave_target(
      in, target,
      "Q", quality,
      "optimize_coding", TRUE,
      "interlace", opts.JpegProgressive,
      NULL);
}

int
vips_jxlsave_go(VipsImage *in, VipsTarget *target, int quality, ImgproxySaveOptions opts)
{
  return vips_jxlsave_target(
      in, target,
      "Q", quality,
      "effort", opts.JxlEffort,
      NULL);
}

int
vips_pngsave_go(VipsImage *in, VipsTarget *target, ImgproxySaveOptions opts)
{
  int quantize = opts.PngQuantize;
  int bitdepth;

  if (quantize) {
    bitdepth = 1;
    if (opts.PngQuantizationColors > 16)
      bitdepth = 8;
    else if (opts.PngQuantizationColors > 4)
      bitdepth = 4;
    else if (opts.PngQuantizationColors > 2)
      bitdepth = 2;
  }
  else {
    bitdepth = vips_get_palette_bit_depth(in);
    if (bitdepth && bitdepth <= 8) {
      if (bitdepth > 4)
        bitdepth = 8;
      else if (bitdepth > 2)
        bitdepth = 4;
      quantize = 1;
    }
  }

  if (!quantize)
    return vips_pngsave_target(
        in, target,
        "filter", VIPS_FOREIGN_PNG_FILTER_ALL,
        "interlace", opts.PngInterlaced,
        NULL);

  return vips_pngsave_target(
      in, target,
      "filter", VIPS_FOREIGN_PNG_FILTER_NONE,
      "interlace", opts.PngInterlaced,
      "palette", quantize,
      "bitdepth", bitdepth,
      NULL);
}

int
vips_webpsave_go(VipsImage *in, VipsTarget *target, int quality, ImgproxySaveOptions opts)
{
  return vips_webpsave_target(
      in, target,
      "Q", quality,
      "effort", opts.WebpEffort,
      "preset", opts.WebpPreset,
      NULL);
}

int
vips_gifsave_go(VipsImage *in, VipsTarget *target, ImgproxySaveOptions opts)
{
  int bitdepth = vips_get_palette_bit_depth(in);
  if (bitdepth <= 0 || bitdepth > 8)
    bitdepth = 8;
  return vips_gifsave_target(in, target, "bitdepth", bitdepth, NULL);
}

int
vips_tiffsave_go(VipsImage *in, VipsTarget *target, int quality, ImgproxySaveOptions opts)
{
  return vips_tiffsave_target(in, target, "Q", quality, NULL);
}

int
vips_heifsave_go(VipsImage *in, VipsTarget *target, int quality, ImgproxySaveOptions opts)
{
  return vips_heifsave_target(
      in, target,
      "Q", quality,
      "bitdepth", 8, // Despite what docs say, >8 bit is not supported
      "compression", VIPS_FOREIGN_HEIF_COMPRESSION_HEVC,
      NULL);
}

int
vips_avifsave_go(VipsImage *in, VipsTarget *target, int quality, ImgproxySaveOptions opts)
{
  return vips_heifsave_target(
      in, target,
      "Q", quality,
      "compression", VIPS_FOREIGN_HEIF_COMPRESSION_AV1,
      "effort", 9 - opts.AvifSpeed,
      NULL);
}

void
vips_cleanup()
{
  vips_error_clear();
  vips_thread_shutdown();
}

void
vips_error_go(const char *function, const char *message)
{
  vips_error(function, "%s", message);
}

int
vips_foreign_load_read_full(VipsSource *source, void *buf, size_t len)
{
  while (len > 0) {
    ssize_t n = vips_source_read(source, buf, len);
    if (n <= 0)
      return n;

    buf = (uint8_t *) buf + n;
    len -= n;
  }

  return 1;
}

void
vips_unref_target(VipsTarget *target)
{
  VIPS_UNREF(target);
}

// vips_linear_go applies out = in * a + b to the colour bands only, leaving any
// alpha channel exactly as it was, and casts the result back to the input's band
// format so the clipping happens once, at the end.
//
// This is the primitive behind `bright` and `contrast`. Both are single linear
// transforms, so the caller folds them into one a/b pair rather than making two
// passes over the pixels.
int
vips_linear_go(VipsImage *in, VipsImage **out, double a, double b)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 6);

  VipsBandFormat format = in->BandFmt;
  VipsInterpretation interpretation = in->Type;

  VipsImage *colour = in;
  VipsImage *alpha = NULL;

  if (vips_image_hasalpha(in)) {
    // Scaling alpha along with the colours would make a brightened image
    // translucent, which is not what either OSS action means.
    if (
        vips_extract_band(in, &t[0], 0, "n", in->Bands - 1, NULL) ||
        vips_extract_band(in, &t[1], in->Bands - 1, "n", 1, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    colour = t[0];
    alpha = t[1];
  }

  if (vips_linear1(colour, &t[2], a, b, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  VipsImage *res = t[2];

  if (alpha != NULL) {
    if (vips_bandjoin2(res, alpha, &t[3], NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    res = t[3];
  }

  // vips_linear works in float; casting back to the input format is what clips
  // an over-bright pixel to white instead of wrapping it around.
  if (vips_cast(res, &t[4], format, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  int ret = vips_copy(t[4], out, "interpretation", interpretation, NULL);

  VIPS_UNREF(base);

  return ret;
}

// vips_round_corners_go builds a rounded-rectangle coverage mask the size of
// `in` and uses it as the image's alpha channel. A radius of half the shorter
// side turns the rectangle into an ellipse, which is how `circle` is built: crop
// to a square first, then call this.
//
// The mask is arithmetic rather than a drawn or rasterised shape, so it needs
// nothing from librsvg and works on any libvips build. It evaluates the signed
// distance to the rounded rectangle,
//
//	d = |max(|p - centre| - inset, 0)| - radius
//
// and turns it into coverage with clamp(radius + 0.5 - d), which leaves a
// one-pixel antialiased band along the arc instead of a staircase.
int
vips_round_corners_go(VipsImage *in, VipsImage **out, double radius)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 20);

  int w = in->Xsize;
  int h = in->Ysize;

  // Distances are measured between pixel centres, so the half-extent of a
  // w-pixel row is (w - 1) / 2 and an even-sided image has its centre between
  // two pixels.
  double cx = (w - 1) / 2.0;
  double cy = (h - 1) / 2.0;

  // Where the straight part of each edge ends and the corner arc begins.
  double bx = cx - radius;
  double by = cy - radius;
  if (bx < 0.0)
    bx = 0.0;
  if (by < 0.0)
    by = 0.0;

  double ones[2] = { 1.0, 1.0 };
  double centre[2] = { -cx, -cy };
  double inset[2] = { -bx, -by };

  if (
      vips_xyz(&t[0], w, h, NULL) ||
      vips_linear(t[0], &t[1], ones, centre, 2, NULL) ||
      vips_abs(t[1], &t[2], NULL) ||
      // q = max(|p - centre| - inset, 0). libvips has no elementwise max with a
      // constant, so multiply by the 0/1 mask of the comparison instead.
      vips_linear(t[2], &t[3], ones, inset, 2, NULL) ||
      vips_moreeq_const1(t[3], &t[4], 0.0, NULL) ||
      vips_linear1(t[4], &t[5], 1.0 / 255.0, 0.0, NULL) ||
      vips_multiply(t[3], t[5], &t[6], NULL) ||
      // |q|, as the square root of the sum of the two squared bands. libvips
      // has no sqrt in VipsOperationMath, so raise to 0.5 instead.
      vips_multiply(t[6], t[6], &t[7], NULL) ||
      vips_extract_band(t[7], &t[8], 0, "n", 1, NULL) ||
      vips_extract_band(t[7], &t[9], 1, "n", 1, NULL) ||
      vips_add(t[8], t[9], &t[10], NULL) ||
      vips_pow_const1(t[10], &t[11], 0.5, NULL) ||
      // coverage = clamp(radius + 0.5 - |q|) * 255; the cast does the clamping.
      vips_linear1(t[11], &t[12], -255.0, (radius + 0.5) * 255.0, NULL) ||
      vips_cast(t[12], &t[13], VIPS_FORMAT_UCHAR, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  VipsImage *mask = t[13];
  VipsImage *colour = in;

  if (vips_image_hasalpha(in)) {
    // Multiply into the existing alpha rather than replacing it, so a source
    // that was already partly transparent stays that way inside the corners.
    if (
        vips_extract_band(in, &t[14], 0, "n", in->Bands - 1, NULL) ||
        vips_extract_band(in, &t[15], in->Bands - 1, "n", 1, NULL) ||
        vips_linear1(t[15], &t[16], 1.0 / 255.0, 0.0, NULL) ||
        vips_multiply(t[16], mask, &t[17], NULL) ||
        vips_cast(t[17], &t[18], VIPS_FORMAT_UCHAR, NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    colour = t[14];
    mask = t[18];
  }

  int ret = vips_bandjoin2(colour, mask, out, NULL);

  VIPS_UNREF(base);

  return ret;
}

// vips_rotate_go rotates by an arbitrary angle, growing the canvas to hold the
// result and leaving the exposed corners transparent.
//
// This is not vips_rot (which the Image.Rotate binding uses): that one is a
// lossless transpose and only accepts multiples of 90. An arbitrary angle is a
// resample, so it needs an interpolator and a background, and the output is the
// bounding box of the rotated rectangle rather than the input size.
//
// Alpha is added first when the image has none, so the corners come out
// transparent instead of black. A format that cannot carry alpha gets them
// flattened to the background colour later in the pipeline, which is what OSS
// does too — white corners on a JPEG, transparent ones on a PNG.
int
vips_rotate_go(VipsImage *in, VipsImage **out, double angle)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 2);

  VipsImage *img = in;

  if (!vips_image_hasalpha(in)) {
    if (vips_addalpha(in, &t[0], NULL)) {
      VIPS_UNREF(base);
      return 1;
    }

    img = t[0];
  }

  double bg[4] = { 0.0, 0.0, 0.0, 0.0 };
  VipsArrayDouble *background = vips_array_double_new(bg, 4);

  int ret = vips_rotate(img, out, angle, "background", background, NULL);

  vips_area_unref(VIPS_AREA(background));
  VIPS_UNREF(base);

  return ret;
}

// vips_ensure_alpha_go adds an opaque alpha channel if the image has none.
//
// Needed before embedding an image into a larger transparent canvas: without an
// alpha band, VIPS_EXTEND_BLACK fills the margin with opaque black instead of
// nothing.
int
vips_ensure_alpha_go(VipsImage *in, VipsImage **out)
{
  if (vips_image_hasalpha(in))
    return vips_copy(in, out, NULL);

  return vips_addalpha(in, out, NULL);
}

// vips_text_go renders a line of text as a coloured RGBA image, optionally with
// a drop shadow.
//
// libvips renders text through Pango, which returns a single-band coverage mask
// rather than a picture: the colour is ours to supply. So the mask becomes the
// alpha channel of a constant-colour image, which is also what makes an
// arbitrary text colour free.
//
// The shadow is a blurred, offset copy of the same mask in black, composited
// underneath. OSS's `shadow_` gives only its transparency; the offset and blur
// are the caller's calibration, scaled from the font size.
int
vips_text_go(VipsImage **out, const char *text, const char *font, int dpi,
    RGB color, double shadow_opacity, int shadow_offset, double shadow_sigma)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 20);

  if (vips_text(&t[0], text, "font", font, "dpi", dpi, "align", VIPS_ALIGN_LOW, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  VipsImage *mask = t[0];

  int w = mask->Xsize;
  int h = mask->Ysize;

  double ones[3] = { 0.0, 0.0, 0.0 };
  double rgb[3] = { (double) color.r, (double) color.g, (double) color.b };

  gboolean with_shadow = shadow_opacity > 0.0 && shadow_offset >= 0;

  int cw = w + (with_shadow ? shadow_offset : 0);
  int ch = h + (with_shadow ? shadow_offset : 0);

  // Constant-colour plane the size of the finished image: black scaled by zero
  // plus the colour as the offset.
  if (
      vips_black(&t[1], cw, ch, "bands", 3, NULL) ||
      vips_linear(t[1], &t[2], ones, rgb, 3, NULL) ||
      vips_cast(t[2], &t[3], VIPS_FORMAT_UCHAR, NULL) ||
      vips_copy(t[3], &t[4], "interpretation", VIPS_INTERPRETATION_sRGB, NULL) ||
      // The glyph coverage, placed at the top left of that plane.
      vips_embed(mask, &t[5], 0, 0, cw, ch, "extend", VIPS_EXTEND_BLACK, NULL) ||
      vips_bandjoin2(t[4], t[5], &t[16], NULL) ||
      // Re-declare the interpretation: bandjoin turns a 3-band sRGB image into
      // a 4-band one and settles on "multiband", which vips_composite2 then
      // refuses to convert into a compositing space.
      vips_copy(t[16], &t[6], "interpretation", VIPS_INTERPRETATION_sRGB, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  if (!with_shadow) {
    int ret = vips_copy(t[6], out, NULL);
    VIPS_UNREF(base);
    return ret;
  }

  double black[3] = { 0.0, 0.0, 0.0 };

  if (
      // Shadow coverage: the same mask, blurred, scaled by the requested
      // opacity, and offset down and right.
      vips_gaussblur(mask, &t[7], shadow_sigma, NULL) ||
      // shadow_opacity is a 0..1 factor, so the blurred coverage keeps its
      // shape and only loses weight.
      vips_linear1(t[7], &t[8], shadow_opacity, 0.0, NULL) ||
      vips_cast(t[8], &t[9], VIPS_FORMAT_UCHAR, NULL) ||
      vips_embed(t[9], &t[10], shadow_offset, shadow_offset, cw, ch,
          "extend", VIPS_EXTEND_BLACK, NULL) ||
      vips_black(&t[11], cw, ch, "bands", 3, NULL) ||
      vips_linear(t[11], &t[12], ones, black, 3, NULL) ||
      vips_cast(t[12], &t[13], VIPS_FORMAT_UCHAR, NULL) ||
      vips_bandjoin2(t[13], t[10], &t[17], NULL) ||
      vips_copy(t[17], &t[14], "interpretation", VIPS_INTERPRETATION_sRGB, NULL) ||
      // Text over shadow.
      vips_composite2(t[14], t[6], &t[15], VIPS_BLEND_MODE_OVER,
          "compositing_space", VIPS_INTERPRETATION_sRGB, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  int ret = vips_copy(t[15], out, NULL);

  VIPS_UNREF(base);

  return ret;
}

// vips_average_hue_go returns the mean colour of the image, in sRGB.
//
// It uses vips_stats rather than three vips_avg calls, and the reason is not
// speed: the JPEG and PNG loaders open their source with VIPS_ACCESS_SEQUENTIAL,
// so the pixels can be walked exactly once. Averaging band by band means three
// walks, which fails outright on those formats. vips_stats does the whole thing
// in one pass and hands back a small table.
//
// The conversion to sRGB first matters: the answer is meant to describe the
// colour a viewer sees, and a CMYK or 16-bit source averaged in its own space
// would give a number in units nobody asked about.
//
// Transparency is weighted, not ignored. A logo on a transparent field is mostly
// stored as transparent black, and a flat mean of those pixels answers "black"
// for an image a person would call green. Premultiplying first and dividing by
// the summed alpha gives the mean of what is actually visible. For an opaque
// image the two are identical, so this only changes the answer where the flat
// mean was wrong.
int
vips_average_hue_go(VipsImage *in, double *r, double *g, double *b)
{
  VipsImage *base = vips_image_new();
  VipsImage **t = (VipsImage **) vips_object_local_array(VIPS_OBJECT(base), 4);

  if (vips_colourspace(in, &t[0], VIPS_INTERPRETATION_sRGB, NULL)) {
    VIPS_UNREF(base);
    return 1;
  }

  VipsImage *img = t[0];
  gboolean has_alpha = vips_image_hasalpha(img);

  if (has_alpha && vips_premultiply(img, &t[1], NULL)) {
    VIPS_UNREF(base);
    return 1;
  }
  if (has_alpha)
    img = t[1];

  if (vips_stats(img, &t[2], NULL) || vips_image_wio_input(t[2])) {
    VIPS_UNREF(base);
    return 1;
  }

  // The stats table is 6 columns wide and one row taller than the band count:
  // row 0 summarises every band together, rows 1..n are the individual bands.
  // Column 2 is the sum and column 4 the mean.
  int bands = t[2]->Ysize - 1;
  double out[3] = { 0.0, 0.0, 0.0 };

  if (has_alpha) {
    double alpha_sum = *((double *) VIPS_IMAGE_ADDR(t[2], 2, bands));

    // Nothing visible at all: there is no colour to report.
    if (alpha_sum > 0.0) {
      for (int i = 0; i < 3; i++) {
        int row = (i < bands - 1 ? i : bands - 2) + 1;
        double premul_sum = *((double *) VIPS_IMAGE_ADDR(t[2], 2, row));

        // premultiply scales by alpha/255, so undoing it needs the same factor.
        out[i] = premul_sum * 255.0 / alpha_sum;
      }
    }
  }
  else {
    for (int i = 0; i < 3; i++) {
      int row = (i < bands ? i : bands - 1) + 1;
      out[i] = *((double *) VIPS_IMAGE_ADDR(t[2], 4, row));
    }
  }

  *r = out[0];
  *g = out[1];
  *b = out[2];

  VIPS_UNREF(base);

  return 0;
}
