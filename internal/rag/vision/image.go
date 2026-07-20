package vision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

type ImageLimits struct {
	MaxSourceBytes  int64
	MaxEncodedBytes int64
	MaxBase64Bytes  int64
	MaxPixels       int64
	MaxEdge         int
}

func (l ImageLimits) validate() error {
	if l.MaxSourceBytes <= 0 || l.MaxEncodedBytes <= 0 || l.MaxBase64Bytes <= 0 || l.MaxPixels <= 0 || l.MaxEdge <= 0 {
		return errors.New("vision: all image normalization limits must be positive")
	}
	return nil
}

// NormalizeImage decodes one bounded raster, discards metadata/extra animation
// frames, composites transparency on white, resizes it and emits a fresh JPEG.
// The returned accounting covers source, encoded and base64-expanded layers.
func NormalizeImage(ctx context.Context, raw []byte, declaredMIME string, limits ImageLimits) (NormalizedImageInput, error) {
	if err := ctx.Err(); err != nil {
		return NormalizedImageInput{}, err
	}
	if err := limits.validate(); err != nil {
		return NormalizedImageInput{}, err
	}
	if len(raw) == 0 || int64(len(raw)) > limits.MaxSourceBytes {
		return NormalizedImageInput{}, fmt.Errorf("vision: source image bytes exceed limit %d", limits.MaxSourceBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return NormalizedImageInput{}, fmt.Errorf("vision: decode image config: %w", err)
	}
	wantMIME, ok := decodedMIME(format)
	if !ok {
		return NormalizedImageInput{}, fmt.Errorf("vision: unsupported raster format %q", format)
	}
	declaredMIME = strings.ToLower(strings.TrimSpace(strings.Split(declaredMIME, ";")[0]))
	if declaredMIME != "" && declaredMIME != wantMIME {
		return NormalizedImageInput{}, fmt.Errorf("vision: declared MIME %q does not match decoded %q", declaredMIME, wantMIME)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > math.MaxInt64/int64(config.Height) ||
		int64(config.Width)*int64(config.Height) > limits.MaxPixels {
		return NormalizedImageInput{}, fmt.Errorf("vision: decoded image pixels exceed limit %d", limits.MaxPixels)
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return NormalizedImageInput{}, fmt.Errorf("vision: decode image: %w", err)
	}
	if decodedFormat != format {
		return NormalizedImageInput{}, errors.New("vision: image format changed between config and decode")
	}

	width, height := fitDimensions(config.Width, config.Height, limits.MaxEdge)
	for {
		if err := ctx.Err(); err != nil {
			return NormalizedImageInput{}, err
		}
		raster, err := safeResize(ctx, decoded, width, height)
		if err != nil {
			return NormalizedImageInput{}, err
		}
		for _, quality := range []int{90, 82, 74, 66, 58, 50, 42, 34} {
			var output bytes.Buffer
			if err := jpeg.Encode(&output, raster, &jpeg.Options{Quality: quality}); err != nil {
				return NormalizedImageInput{}, fmt.Errorf("vision: encode normalized JPEG: %w", err)
			}
			encodedSize := int64(output.Len())
			base64Size := int64(base64.StdEncoding.EncodedLen(output.Len()))
			if encodedSize <= limits.MaxEncodedBytes && base64Size <= limits.MaxBase64Bytes {
				data := append([]byte(nil), output.Bytes()...)
				sum := sha256.Sum256(data)
				return NormalizedImageInput{
					Bytes: data, MIMEType: "image/jpeg", Width: width, Height: height,
					SHA256: hex.EncodeToString(sum[:]), Base64Bytes: base64Size,
				}, nil
			}
		}
		if width == 1 && height == 1 {
			break
		}
		width = max(1, width*3/4)
		height = max(1, height*3/4)
	}
	return NormalizedImageInput{}, errors.New("vision: normalized image cannot fit encoded/base64 limits")
}

func decodedMIME(format string) (string, bool) {
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

func fitDimensions(width, height, maxEdge int) (int, int) {
	if width <= maxEdge && height <= maxEdge {
		return width, height
	}
	if width >= height {
		return maxEdge, max(1, int(math.Round(float64(height)*float64(maxEdge)/float64(width))))
	}
	return max(1, int(math.Round(float64(width)*float64(maxEdge)/float64(height)))), maxEdge
}

func safeResize(ctx context.Context, source image.Image, width, height int) (*image.RGBA, error) {
	if width <= 0 || height <= 0 {
		return nil, errors.New("vision: invalid resize dimensions")
	}
	bounds := source.Bounds()
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		if y&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		sy := bounds.Min.Y + y*bounds.Dy()/height
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*bounds.Dx()/width
			pixel := color.NRGBAModel.Convert(source.At(sx, sy)).(color.NRGBA)
			alpha := uint32(pixel.A)
			blend := func(component uint8) uint8 {
				return uint8((uint32(component)*alpha + 255*(255-alpha) + 127) / 255)
			}
			result.SetRGBA(x, y, color.RGBA{R: blend(pixel.R), G: blend(pixel.G), B: blend(pixel.B), A: 255})
		}
	}
	return result, nil
}
