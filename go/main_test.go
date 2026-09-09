package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, args := range [][]string{
		{"-m", "flash-lite", "a cat", "-a", "9:16"},
		{"a cat", "--model=flash-lite", "--aspect=9:16"},
	} {
		o, err := parseArgs(args)
		if err != nil || o.model != models["flash-lite"] || o.prompt != "a cat" || o.aspect != "9:16" {
			t.Fatalf("parse %v: %+v, %v", args, o, err)
		}
	}
	o, err := parseArgs([]string{"--", "-a prompt"})
	if err != nil || o.prompt != "-a prompt" {
		t.Fatalf("literal prompt: %+v %v", o, err)
	}
	for _, args := range [][]string{nil, {""}, {"cat", "dog"}, {"cat", "--wat"}, {"cat", "-m"}, {"cat", "--aspect=8:9"}, {"cat", "--transparent", "-o", "a.jpg"}, {"cat", "-o", "a.webp"}, {"cat", "--model=../bad"}} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted invalid args: %v", args)
		}
	}
	if o, err := parseArgs([]string{"--help"}); err != nil || !o.help {
		t.Fatal("help should not require prompt")
	}
}

type recordedCall struct {
	model, prompt, aspect string
	images                []imageData
}
type fakeGenerator struct {
	calls   []recordedCall
	results []imageData
	err     error
}

func (f *fakeGenerator) generate(_ context.Context, model, prompt string, images []imageData, aspect string) (imageData, error) {
	f.calls = append(f.calls, recordedCall{model, prompt, aspect, images})
	if f.err != nil {
		return imageData{}, f.err
	}
	if len(f.calls) > len(f.results) {
		return imageData{}, errors.New("unexpected extra API call")
	}
	return f.results[len(f.calls)-1], nil
}

func fixtureImage(t *testing.T, c color.NRGBA) imageData {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			im.SetNRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return imageData{Data: b.Bytes(), MIMEType: "image/png"}
}

func TestDirectPipeline(t *testing.T) {
	dir := t.TempDir()
	im := fixtureImage(t, color.NRGBA{R: 100, A: 255})
	input := filepath.Join(dir, "input.png")
	if err := os.WriteFile(input, im.Data, 0600); err != nil {
		t.Fatal(err)
	}
	f := &fakeGenerator{results: []imageData{im}}
	o := options{prompt: "make blue", model: models["flash-lite"], input: input, output: filepath.Join(dir, "out.png"), aspect: "4:3", timings: true}
	var log bytes.Buffer
	if err := run(context.Background(), o, f, &log); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || len(f.calls[0].images) != 1 || f.calls[0].model != o.model || f.calls[0].aspect != "4:3" {
		t.Fatalf("calls: %+v", f.calls)
	}
	got, err := os.ReadFile(o.output)
	if err != nil || !bytes.Equal(got, im.Data) {
		t.Fatal("direct output did not preserve original bytes", err)
	}
	if !strings.Contains(log.String(), "Timing: API") {
		t.Fatal("missing timing output")
	}
}

func TestTransparentPipeline(t *testing.T) {
	white := fixtureImage(t, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	black := fixtureImage(t, color.NRGBA{A: 255})
	f := &fakeGenerator{results: []imageData{white, black}}
	o := options{prompt: "a vase", model: defaultModel, output: filepath.Join(t.TempDir(), "vase.png"), transparent: true, intermediates: true, aspect: "1:1"}
	if err := run(context.Background(), o, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || len(f.calls[1].images) != 1 || !bytes.Equal(f.calls[1].images[0].Data, white.Data) {
		t.Fatal("second call must edit first result")
	}
	if f.calls[0].aspect != "1:1" || f.calls[1].aspect != "1:1" {
		t.Fatal("aspect ratio lost")
	}
	wp, bp := intermediatePaths(o.output)
	for _, p := range []string{o.output, wp, bp} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Open(o.output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a := img.At(0, 0).RGBA()
	if a != 0 {
		t.Fatalf("background alpha=%d", a)
	}
}

func TestPipelineFailureDoesNotWriteOutput(t *testing.T) {
	o := options{prompt: "cat", model: defaultModel, output: filepath.Join(t.TempDir(), "out.png")}
	f := &fakeGenerator{err: errors.New("API unavailable")}
	if err := run(context.Background(), o, f, io.Discard); err == nil {
		t.Fatal("missing error")
	}
	if _, err := os.Stat(o.output); !os.IsNotExist(err) {
		t.Fatal("output exists after failure")
	}
	o.input = "missing.png"
	f.calls = nil
	if err := run(context.Background(), o, f, io.Discard); err == nil || len(f.calls) != 0 {
		t.Fatal("bad input should fail before API call")
	}
}
