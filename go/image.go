package main

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

func loadImage(path string) (imageData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return imageData{}, fmt.Errorf("read image %q: %w", path, err)
	}

	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return imageData{}, fmt.Errorf("decode image %q: %w", path, err)
	}
	mimeType, ok := mimeForFormat(format)
	if !ok {
		return imageData{}, fmt.Errorf("unsupported image format %q in %q (use PNG, JPEG, or GIF)", format, path)
	}
	// Gemini accepts PNG and JPEG uploads, but not GIF. Convert the first GIF
	// frame once while loading; PNG and JPEG retain their original upload bytes.
	if format == "gif" {
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return imageData{}, fmt.Errorf("decode GIF %q: %w", path, err)
		}
		var pngData bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := encoder.Encode(&pngData, decoded); err != nil {
			return imageData{}, fmt.Errorf("encode GIF %q as PNG: %w", path, err)
		}
		return imageData{Data: pngData.Bytes(), MIMEType: "image/png"}, nil
	}
	return imageData{Data: data, MIMEType: mimeType}, nil
}

func saveImage(path string, data imageData) error {
	ext := strings.ToLower(filepath.Ext(path))
	var format, wantedMIME string
	switch ext {
	case ".png":
		format, wantedMIME = "png", "image/png"
	case ".jpg", ".jpeg":
		format, wantedMIME = "jpeg", "image/jpeg"
	default:
		return fmt.Errorf("unsupported output extension %q (use .png, .jpg, or .jpeg)", ext)
	}

	if data.MIMEType == wantedMIME {
		if err := os.WriteFile(path, data.Data, 0o644); err != nil {
			return fmt.Errorf("write image %q: %w", path, err)
		}
		return nil
	}

	src, _, err := image.Decode(bytes.NewReader(data.Data))
	if err != nil {
		return fmt.Errorf("decode image for %q: %w", path, err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create image %q: %w", path, err)
	}
	if format == "png" {
		err = png.Encode(f, src)
	} else {
		err = jpeg.Encode(f, src, &jpeg.Options{Quality: 95})
	}
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("encode image %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close image %q: %w", path, err)
	}
	return nil
}

func matte(white, black imageData) (imageData, error) {
	whiteImage, _, err := image.Decode(bytes.NewReader(white.Data))
	if err != nil {
		return imageData{}, fmt.Errorf("decode white image: %w", err)
	}
	blackImage, _, err := image.Decode(bytes.NewReader(black.Data))
	if err != nil {
		return imageData{}, fmt.Errorf("decode black image: %w", err)
	}

	whiteRGBA := asNRGBA(whiteImage)
	blackRGBA := asNRGBA(blackImage)
	if blackRGBA.Bounds().Dx() != whiteRGBA.Bounds().Dx() || blackRGBA.Bounds().Dy() != whiteRGBA.Bounds().Dy() {
		blackRGBA = resizeBilinear(blackRGBA, whiteRGBA.Bounds().Dx(), whiteRGBA.Bounds().Dy())
	}

	result := image.NewNRGBA(whiteRGBA.Bounds())
	height := result.Bounds().Dy()
	workers := 1
	if pixels := result.Bounds().Dx() * height; pixels >= 256*1024 {
		workers = min(runtime.GOMAXPROCS(0), height)
	}
	if workers == 1 {
		matteRows(result, whiteRGBA, blackRGBA, 0, height)
	} else {
		var wg sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			y0 := height * worker / workers
			y1 := height * (worker + 1) / workers
			wg.Add(1)
			go func() {
				defer wg.Done()
				matteRows(result, whiteRGBA, blackRGBA, y0, y1)
			}()
		}
		wg.Wait()
	}

	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&encoded, result); err != nil {
		return imageData{}, fmt.Errorf("encode matte: %w", err)
	}
	return imageData{Data: encoded.Bytes(), MIMEType: "image/png"}, nil
}

func matteRows(dst, white, black *image.NRGBA, y0, y1 int) {
	width := dst.Bounds().Dx()
	backgroundDistance := math.Sqrt(3 * 255 * 255)
	for y := y0; y < y1; y++ {
		for x := 0; x < width; x++ {
			offset := y*dst.Stride + x*4
			rW, gW, bW := float64(white.Pix[offset]), float64(white.Pix[offset+1]), float64(white.Pix[offset+2])
			rB, gB, bB := float64(black.Pix[offset]), float64(black.Pix[offset+1]), float64(black.Pix[offset+2])
			pixelDistance := math.Sqrt((rW-rB)*(rW-rB) + (gW-gB)*(gW-gB) + (bW-bB)*(bW-bB))
			alpha := min(1, max(0, 1-pixelDistance/backgroundDistance))

			var rOut, gOut, bOut float64
			if alpha > 0.01 {
				rOut, gOut, bOut = rB/alpha, gB/alpha, bB/alpha
			} else {
				alpha = 0
			}
			dst.Pix[offset] = uint8(min(255, rOut))
			dst.Pix[offset+1] = uint8(min(255, gOut))
			dst.Pix[offset+2] = uint8(min(255, bOut))
			dst.Pix[offset+3] = uint8(alpha * 255)
		}
	}
}

func asNRGBA(src image.Image) *image.NRGBA {
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	return dst
}

func resizeBilinear(src *image.NRGBA, width, height int) *image.NRGBA {
	if width <= 0 || height <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, width, height))
	}
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sy := (float64(y)+0.5)*float64(sh)/float64(height) - 0.5
		sy = min(float64(sh-1), max(0, sy))
		y0 := int(math.Floor(sy))
		y1 := min(sh-1, y0+1)
		fy := sy - math.Floor(sy)
		for x := 0; x < width; x++ {
			sx := (float64(x)+0.5)*float64(sw)/float64(width) - 0.5
			sx = min(float64(sw-1), max(0, sx))
			x0 := int(math.Floor(sx))
			x1 := min(sw-1, x0+1)
			fx := sx - math.Floor(sx)
			for c := 0; c < 4; c++ {
				p00 := float64(src.Pix[y0*src.Stride+x0*4+c])
				p10 := float64(src.Pix[y0*src.Stride+x1*4+c])
				p01 := float64(src.Pix[y1*src.Stride+x0*4+c])
				p11 := float64(src.Pix[y1*src.Stride+x1*4+c])
				top := p00 + (p10-p00)*fx
				bottom := p01 + (p11-p01)*fx
				dst.Pix[y*dst.Stride+x*4+c] = uint8(math.Round(top + (bottom-top)*fy))
			}
		}
	}
	return dst
}

func intermediatePaths(output string) (string, string) {
	dir, name := filepath.Dir(output), filepath.Base(output)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	return filepath.Join(dir, base+"-white.png"), filepath.Join(dir, base+"-black.png")
}

func mimeForFormat(format string) (string, bool) {
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
