package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 80, A: uint8(100 + (x+y)%155)})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNormalizeImageResizesReencodesAndBoundsAllLayers(t *testing.T) {
	raw := testPNG(t, 64, 32)
	input, err := NormalizeImage(context.Background(), raw, "image/png", ImageLimits{
		MaxSourceBytes: int64(len(raw) + 1), MaxEncodedBytes: 32 << 10,
		MaxBase64Bytes: 48 << 10, MaxPixels: 64 * 32, MaxEdge: 24,
	})
	if err != nil {
		t.Fatalf("normalize image: %v", err)
	}
	if input.MIMEType != "image/jpeg" || input.Width != 24 || input.Height != 12 {
		t.Fatalf("normalized = mime %s %dx%d", input.MIMEType, input.Width, input.Height)
	}
	if _, err := jpeg.Decode(bytes.NewReader(input.Bytes)); err != nil {
		t.Fatalf("normalized bytes are not JPEG: %v", err)
	}
	if input.SHA256 == "" || input.Base64Bytes > 48<<10 {
		t.Fatalf("hash/base64 accounting missing: %+v", input)
	}

	if _, err := NormalizeImage(context.Background(), raw, "image/jpeg", ImageLimits{
		MaxSourceBytes: 1 << 20, MaxEncodedBytes: 1 << 20, MaxBase64Bytes: 2 << 20,
		MaxPixels: 10_000, MaxEdge: 100,
	}); err == nil {
		t.Fatal("expected declared MIME mismatch")
	}
	if _, err := NormalizeImage(context.Background(), raw, "image/png", ImageLimits{
		MaxSourceBytes: int64(len(raw) - 1), MaxEncodedBytes: 1 << 20,
		MaxBase64Bytes: 2 << 20, MaxPixels: 10_000, MaxEdge: 100,
	}); err == nil {
		t.Fatal("expected raw byte limit")
	}
	if _, err := NormalizeImage(context.Background(), raw, "image/png", ImageLimits{
		MaxSourceBytes: 1 << 20, MaxEncodedBytes: 1 << 20,
		MaxBase64Bytes: 2 << 20, MaxPixels: 100, MaxEdge: 100,
	}); err == nil {
		t.Fatal("expected decoded pixel limit")
	}
}

func TestNormalizeImageHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NormalizeImage(ctx, testPNG(t, 8, 8), "image/png", ImageLimits{
		MaxSourceBytes: 1 << 20, MaxEncodedBytes: 1 << 20,
		MaxBase64Bytes: 2 << 20, MaxPixels: 1000, MaxEdge: 100,
	}); err == nil {
		t.Fatal("expected cancellation")
	}
}
