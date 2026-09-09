package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestMatteRecoversOpaqueTranslucentAndTransparentPixels(t *testing.T) {
	object := []color.NRGBA{
		{R: 20, G: 80, B: 150, A: 255},
		{R: 30, G: 100, B: 200, A: 128},
		{R: 0, G: 0, B: 0, A: 0},
	}
	white := image.NewNRGBA(image.Rect(0, 0, len(object), 1))
	black := image.NewNRGBA(white.Bounds())
	for x, pixel := range object {
		white.SetNRGBA(x, 0, composite(pixel, color.NRGBA{R: 255, G: 255, B: 255, A: 255}))
		black.SetNRGBA(x, 0, composite(pixel, color.NRGBA{A: 255}))
	}

	output, err := matte(encodePNG(t, white), encodePNG(t, black))
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeNRGBA(t, output)
	assertNearColor(t, decoded.NRGBAAt(0, 0), object[0], 1)
	assertNearColor(t, decoded.NRGBAAt(1, 0), object[1], 3)
	assertNearColor(t, decoded.NRGBAAt(2, 0), object[2], 0)
}

func TestMatteResizesBlackImage(t *testing.T) {
	white := image.NewNRGBA(image.Rect(0, 0, 4, 3))
	black := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			white.SetNRGBA(x, y, color.NRGBA{R: 143, G: 178, B: 228, A: 255})
		}
	}
	black.SetNRGBA(0, 0, color.NRGBA{R: 15, G: 50, B: 100, A: 255})

	output, err := matte(encodePNG(t, white), encodePNG(t, black))
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeNRGBA(t, output)
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			assertNearColor(t, decoded.NRGBAAt(x, y), color.NRGBA{R: 30, G: 100, B: 200, A: 128}, 3)
		}
	}
}

func TestLoadAndSaveImageFormats(t *testing.T) {
	dir := t.TempDir()
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 255})

	formats := []struct {
		ext, mime string
		encode    func(*os.File) error
	}{
		{".png", "image/png", func(f *os.File) error { return png.Encode(f, src) }},
		{".jpg", "image/jpeg", func(f *os.File) error { return jpeg.Encode(f, src, nil) }},
		{".gif", "image/png", func(f *os.File) error { return gif.Encode(f, src, nil) }},
	}
	for _, tc := range formats {
		path := filepath.Join(dir, "input"+tc.ext)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := tc.encode(f); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadImage(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.MIMEType != tc.mime {
			t.Errorf("%s MIME = %q, want %q", tc.ext, loaded.MIMEType, tc.mime)
		}
	}

	pngData := encodePNG(t, src)
	pngPath := filepath.Join(dir, "unchanged.png")
	if err := saveImage(pngPath, pngData); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, pngData.Data) {
		t.Error("matching PNG should be written without re-encoding")
	}
	if err := saveImage(filepath.Join(dir, "converted.jpg"), pngData); err != nil {
		t.Fatal(err)
	}
	if _, format, err := image.DecodeConfig(mustRead(t, filepath.Join(dir, "converted.jpg"))); err != nil || format != "jpeg" {
		t.Errorf("converted JPEG format = %q, err = %v", format, err)
	}
	if err := saveImage(filepath.Join(dir, "bad.webp"), pngData); err == nil {
		t.Error("unsupported output extension was accepted")
	}
}

func TestIntermediatePaths(t *testing.T) {
	white, black := intermediatePaths("images/result.jpg")
	if white != filepath.Join("images", "result-white.png") || black != filepath.Join("images", "result-black.png") {
		t.Fatalf("got %q and %q", white, black)
	}
}

func BenchmarkMatte1024(b *testing.B) {
	const size = 1024
	white := image.NewNRGBA(image.Rect(0, 0, size, size))
	black := image.NewNRGBA(white.Bounds())
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			white.SetNRGBA(x, y, color.NRGBA{R: 143, G: 178, B: 228, A: 255})
			black.SetNRGBA(x, y, color.NRGBA{R: 15, G: 50, B: 100, A: 255})
		}
	}
	whiteData, blackData := encodePNG(b, white), encodePNG(b, black)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := matte(whiteData, blackData); err != nil {
			b.Fatal(err)
		}
	}
}

func encodePNG(t testing.TB, img image.Image) imageData {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return imageData{Data: buf.Bytes(), MIMEType: "image/png"}
}

func decodeNRGBA(t testing.TB, data imageData) *image.NRGBA {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data.Data))
	if err != nil {
		t.Fatal(err)
	}
	return asNRGBA(img)
}

func composite(foreground, background color.NRGBA) color.NRGBA {
	a := int(foreground.A)
	return color.NRGBA{
		R: uint8((int(foreground.R)*a + int(background.R)*(255-a) + 127) / 255),
		G: uint8((int(foreground.G)*a + int(background.G)*(255-a) + 127) / 255),
		B: uint8((int(foreground.B)*a + int(background.B)*(255-a) + 127) / 255),
		A: 255,
	}
}

func assertNearColor(t testing.TB, got, want color.NRGBA, tolerance uint8) {
	t.Helper()
	for _, pair := range [][2]uint8{{got.R, want.R}, {got.G, want.G}, {got.B, want.B}, {got.A, want.A}} {
		difference := int(pair[0]) - int(pair[1])
		if difference < 0 {
			difference = -difference
		}
		if difference > int(tolerance) {
			t.Fatalf("got %#v, want %#v (tolerance %d)", got, want, tolerance)
		}
	}
}

func mustRead(t testing.TB, path string) *bytes.Reader {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(data)
}
