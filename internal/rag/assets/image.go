package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

var ErrUnsupportedRaster = errors.New("rag assets: no approved safe raster decoder")

// SafeImageLimits applies before and after decode. MaxSourceBytes bounds the
// encoded input, MaxPixels prevents decompression bombs, and the edge limits
// bound the freshly encoded browser variants.
type SafeImageLimits struct {
	MaxSourceBytes   int64
	MaxPixels        int64
	DisplayMaxEdge   int
	ThumbnailMaxEdge int
}

func (l SafeImageLimits) normalized() (SafeImageLimits, error) {
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = 20 << 20
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = 40_000_000
	}
	if l.DisplayMaxEdge <= 0 {
		l.DisplayMaxEdge = 2400
	}
	if l.ThumbnailMaxEdge <= 0 {
		l.ThumbnailMaxEdge = min(480, l.DisplayMaxEdge)
	}
	if l.ThumbnailMaxEdge > l.DisplayMaxEdge {
		return SafeImageLimits{}, errors.New("rag assets: thumbnail edge exceeds display edge")
	}
	return l, nil
}

// EncodedRaster is a freshly encoded, metadata-free, single-frame PNG.
type EncodedRaster struct {
	Bytes    []byte
	MIMEType string
	SHA256   string
	Width    int
	Height   int
}

type DisplayVariants struct {
	Display   EncodedRaster
	Thumbnail EncodedRaster
}

// SafeRasterSupported reports formats for which this build has an approved
// bounded decoder. SVG/EMF/WMF/PDF XObjects and unknown image/* formats must
// remain source-only with display_status=unavailable.
func SafeRasterSupported(mimeType string) bool {
	switch canonicalImageMIME(mimeType) {
	case "image/png", "image/jpeg", "image/gif":
		return true
	default:
		return false
	}
}

// MakeDisplayVariants decodes only approved raster formats, enforces the
// configured pixel limit before full decode, drops metadata/animation frames,
// and emits deterministic PNG display and thumbnail variants.
func MakeDisplayVariants(ctx context.Context, raw []byte, declaredMIME string, limits SafeImageLimits) (DisplayVariants, error) {
	limits, err := limits.normalized()
	if err != nil {
		return DisplayVariants{}, err
	}
	decoded, err := decodeSafeRaster(ctx, raw, declaredMIME, limits)
	if err != nil {
		return DisplayVariants{}, err
	}
	displayWidth, displayHeight := fitImageDimensions(decoded.Bounds().Dx(), decoded.Bounds().Dy(), limits.DisplayMaxEdge)
	display, err := encodeResizedPNG(ctx, decoded, displayWidth, displayHeight)
	if err != nil {
		return DisplayVariants{}, err
	}
	thumbWidth, thumbHeight := fitImageDimensions(decoded.Bounds().Dx(), decoded.Bounds().Dy(), limits.ThumbnailMaxEdge)
	thumbnail, err := encodeResizedPNG(ctx, decoded, thumbWidth, thumbHeight)
	if err != nil {
		return DisplayVariants{}, err
	}
	return DisplayVariants{Display: display, Thumbnail: thumbnail}, nil
}

// CropRaster creates a safe source asset from a normalized page bbox. It
// never returns a view into the untrusted decoder buffer and always performs a
// fresh PNG encode so page metadata cannot leak into the derived crop.
func CropRaster(ctx context.Context, raw []byte, declaredMIME string, bbox document.NormalizedBBox, limits SafeImageLimits) (EncodedRaster, error) {
	if err := bbox.Validate(); err != nil {
		return EncodedRaster{}, err
	}
	limits, err := limits.normalized()
	if err != nil {
		return EncodedRaster{}, err
	}
	decoded, err := decodeSafeRaster(ctx, raw, declaredMIME, limits)
	if err != nil {
		return EncodedRaster{}, err
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	x0 := bounds.Min.X + bbox[0]*width/1000
	y0 := bounds.Min.Y + bbox[1]*height/1000
	x1 := bounds.Min.X + (bbox[2]*width+999)/1000
	y1 := bounds.Min.Y + (bbox[3]*height+999)/1000
	x0 = min(max(x0, bounds.Min.X), bounds.Max.X-1)
	y0 = min(max(y0, bounds.Min.Y), bounds.Max.Y-1)
	x1 = min(max(x1, x0+1), bounds.Max.X)
	y1 = min(max(y1, y0+1), bounds.Max.Y)
	cropWidth, cropHeight := x1-x0, y1-y0
	crop := image.NewNRGBA(image.Rect(0, 0, cropWidth, cropHeight))
	for y := 0; y < cropHeight; y++ {
		if y&63 == 0 {
			if err := ctx.Err(); err != nil {
				return EncodedRaster{}, err
			}
		}
		for x := 0; x < cropWidth; x++ {
			crop.Set(x, y, decoded.At(x0+x, y0+y))
		}
	}
	return encodePNG(ctx, crop)
}

func decodeSafeRaster(ctx context.Context, raw []byte, declaredMIME string, limits SafeImageLimits) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) == 0 || int64(len(raw)) > limits.MaxSourceBytes {
		return nil, fmt.Errorf("rag assets: source image exceeds %d bytes", limits.MaxSourceBytes)
	}
	declaredMIME = canonicalImageMIME(declaredMIME)
	if !SafeRasterSupported(declaredMIME) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedRaster, declaredMIME)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("rag assets: decode raster header: %w", err)
	}
	decodedMIME, ok := decodedImageMIME(format)
	if !ok || decodedMIME != declaredMIME {
		return nil, fmt.Errorf("rag assets: declared MIME %q does not match decoded raster %q", declaredMIME, format)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > math.MaxInt64/int64(config.Height) ||
		int64(config.Width)*int64(config.Height) > limits.MaxPixels {
		return nil, fmt.Errorf("rag assets: decoded image exceeds %d pixels", limits.MaxPixels)
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("rag assets: decode raster: %w", err)
	}
	if decodedFormat != format {
		return nil, errors.New("rag assets: raster format changed between header and decode")
	}
	return decoded, nil
}

func canonicalImageMIME(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
}

func decodedImageMIME(format string) (string, bool) {
	switch format {
	case "png":
		return "image/png", true
	case "jpeg":
		return "image/jpeg", true
	case "gif":
		return "image/gif", true
	default:
		return "", false
	}
}

func fitImageDimensions(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}
	if width >= height {
		return maxEdge, max(1, int(math.Round(float64(height)*float64(maxEdge)/float64(width))))
	}
	return max(1, int(math.Round(float64(width)*float64(maxEdge)/float64(height)))), maxEdge
}

func encodeResizedPNG(ctx context.Context, source image.Image, width, height int) (EncodedRaster, error) {
	if width <= 0 || height <= 0 {
		return EncodedRaster{}, errors.New("rag assets: invalid output dimensions")
	}
	bounds := source.Bounds()
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		if y&63 == 0 {
			if err := ctx.Err(); err != nil {
				return EncodedRaster{}, err
			}
		}
		sy := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*bounds.Dx()/width
			result.SetNRGBA(x, y, color.NRGBAModel.Convert(source.At(sx, sy)).(color.NRGBA))
		}
	}
	return encodePNG(ctx, result)
}

func encodePNG(ctx context.Context, source image.Image) (EncodedRaster, error) {
	if err := ctx.Err(); err != nil {
		return EncodedRaster{}, err
	}
	var output bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&output, source); err != nil {
		return EncodedRaster{}, fmt.Errorf("rag assets: encode safe PNG: %w", err)
	}
	data := append([]byte(nil), output.Bytes()...)
	hash := sha256.Sum256(data)
	return EncodedRaster{
		Bytes: data, MIMEType: "image/png", SHA256: hex.EncodeToString(hash[:]),
		Width: source.Bounds().Dx(), Height: source.Bounds().Dy(),
	}, nil
}
