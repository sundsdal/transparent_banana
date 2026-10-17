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

func TestResolveAPIKey(t *testing.T) {
	previous := bundledAPIKey
	t.Cleanup(func() { bundledAPIKey = previous })
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("NANOBANANA_BUNDLE_KEY", "")
	bundledAPIKey = "embedded-test-key"
	if resolveAPIKey() != "embedded-test-key" {
		t.Fatal("embedded key must work without runtime environment")
	}
	t.Setenv("GEMINI_API_KEY", "runtime-test-key")
	if resolveAPIKey() != "runtime-test-key" {
		t.Fatal("runtime GEMINI_API_KEY must override embedded key")
	}
	t.Setenv("GEMINI_API_KEY", "")
	bundledAPIKey = ""
	t.Setenv("NANOBANANA_BUNDLE_KEY", "fallback-test-key")
	if resolveAPIKey() != "fallback-test-key" {
		t.Fatal("missing runtime fallback")
	}
}

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

func TestParseVideoArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--video", "a cat"},
		{"a cat", "-m", "omni"},
		{"a cat", "--model=gemini-omni-1.1-flash"},
	} {
		o, err := parseArgs(args)
		if err != nil || !o.video || o.model != defaultVideoModel || o.output != "output.mp4" {
			t.Fatalf("parse %v: %+v, %v", args, o, err)
		}
	}
	o, err := parseArgs([]string{"a cat", "--video", "-m", "omni", "-o", "CAT.MP4", "-a", "9:16", "--resolution=4k", "--duration=8"})
	if err != nil || o.output != "CAT.MP4" || o.aspect != "9:16" || o.resolution != "4k" || o.duration != "8s" {
		t.Fatalf("explicit video args: %+v, %v", o, err)
	}
	for _, input := range []string{"3", "10", "8s", "03s"} {
		o, err := parseArgs([]string{"cat", "--video", "--duration", input})
		if err != nil || o.duration == "" || !strings.HasSuffix(o.duration, "s") {
			t.Fatalf("duration %q: %+v, %v", input, o, err)
		}
	}
	if o, err := parseArgs([]string{"cat", "--video"}); err != nil || o.duration != "" {
		t.Fatalf("default duration should be omitted: %+v, %v", o, err)
	}
	for _, args := range [][]string{
		{"cat", "--video", "-m", "flash"},
		{"cat", "-m", "omni", "-o", "cat.png"},
		{"cat", "--video", "--transparent"},
		{"cat", "--video", "--save-intermediates"},
		{"cat", "--video", "-a", "1:1"},
		{"cat", "--video", "-a", "9:16 16:9"},
		{"cat", "--video", "--resolution", "360p 720p"},
		{"cat", "--video", "--resolution", "bad"},
		{"cat", "--resolution", "720p"},
		{"cat", "--duration", "8"},
		{"cat", "--video", "--duration", "0"},
		{"cat", "--video", "--duration", "2"},
		{"cat", "--video", "--duration", "11"},
		{"cat", "--video", "--duration", "8.5"},
		{"cat", "--video", "--duration", "-8"},
		{"cat", "--video", "--duration", "+8"},
		{"cat", "--video", "--duration", "8ms"},
		{"cat", "--video", "--duration", "eight"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted invalid video args: %v", args)
		}
	}
}

func TestParseAdvancedImageArgs(t *testing.T) {
	o, err := parseArgs([]string{"-i", "a.png,b.jpg", "make art", "-i", "c.webp", "--resolution", "4K", "--thinking", "medium", "--include-thoughts", "--search", "both", "--text", "--session", "turns.json", "--response-json", "response.json"})
	if err != nil || o.video || o.input != "a.png,b.jpg,c.webp" || o.imageSize != "4K" || o.thinking != "medium" || !o.includeThoughts || o.search != "both" || !o.text || o.session != "turns.json" || o.responseJSON != "response.json" {
		t.Fatalf("advanced args = %+v, %v", o, err)
	}
	for _, args := range [][]string{
		{"cat", "--image-size", "3K"},
		{"cat", "--resolution", "720p"},
		{"cat", "--image-size", "2K", "--resolution", "4K"},
		{"cat", "--thinking", "low"},
		{"cat", "--search", "videos"},
		{"cat", "--session", "turns.json", "--transparent"},
		{"cat", "-m", "flash-lite", "--thinking", "high"},
		{"cat", "-m", "omni", "--thinking", "high"},
		{"cat", "-m", "gemini-omni-1.1-flash", "--text"},
		{"cat", "--video", "--image-size", "2K"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted invalid advanced args: %v", args)
		}
	}
}

func TestImageArtifactPathCollisions(t *testing.T) {
	for _, tc := range []struct {
		name, output, flag, path string
		extra                    []string
		collision                bool
	}{
		{name: "manifest alias", output: "result.png", flag: "--response-json", path: "nested/../result.md", collision: true},
		{name: "numbered image", output: "result.png", flag: "--session", path: "result-2.png", collision: true},
		{name: "large image number", output: "result.JPG", flag: "--response-json", path: "result-123.JPG", collision: true},
		{name: "empty stem numbered image alias", output: ".png", flag: "--response-json", path: "nested/../-2.png", collision: true},
		{name: "dot stem numbered image", output: "..png", flag: "--session", path: ".-2.png", collision: true},
		{name: "grounding without search", output: "result.png", flag: "--session", path: "result-grounding.html", collision: true},
		{name: "thought markdown", output: "result.png", flag: "--response-json", path: "result-thoughts.md", extra: []string{"--include-thoughts"}, collision: true},
		{name: "thought image", output: "result.png", flag: "--session", path: "result-thought-12.png", extra: []string{"--include-thoughts"}, collision: true},
		{name: "transparent white", output: "result.png", flag: "--response-json", path: "result-white.png", extra: []string{"--transparent", "--save-intermediates"}, collision: true},
		{name: "transparent black", output: "result.png", flag: "--response-json", path: "result-black.png", extra: []string{"--transparent", "--save-intermediates"}, collision: true},
		{name: "transparent white grounding", output: "result.png", flag: "--response-json", path: "result-white-grounding.html", extra: []string{"--transparent"}, collision: true},
		{name: "transparent black grounding", output: "result.png", flag: "--response-json", path: "result-black-grounding.html", extra: []string{"--transparent"}, collision: true},
		{name: "transparent thought image", output: "result.png", flag: "--response-json", path: "result-thought-1.png", extra: []string{"--transparent", "--include-thoughts"}, collision: true},
		{name: "inactive thought file", output: "result.png", flag: "--response-json", path: "result-thoughts.md"},
		{name: "inactive intermediate", output: "result.png", flag: "--response-json", path: "result-white.png", extra: []string{"--transparent"}},
		{name: "transparent single final image", output: "result.png", flag: "--response-json", path: "result-2.png", extra: []string{"--transparent"}},
		{name: "noncanonical image number", output: "result.png", flag: "--response-json", path: "result-02.png"},
		{name: "signed image number", output: "result.png", flag: "--response-json", path: "result-+2.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"prompt", "-o", tc.output, tc.flag, tc.path}, tc.extra...)
			_, err := parseArgs(args)
			if (err != nil) != tc.collision {
				t.Fatalf("parseArgs(%q) error = %v, want collision %t", args, err, tc.collision)
			}
		})
	}
}

type fakeVideoGenerator struct {
	fakeGenerator
	videoCalls []struct {
		model, prompt, aspect, resolution, duration string
		images                                      []imageData
	}
	video    []byte
	videoErr error
}

func (f *fakeVideoGenerator) generateVideo(_ context.Context, model, prompt string, images []imageData, aspect, resolution, duration string) ([]byte, error) {
	f.videoCalls = append(f.videoCalls, struct {
		model, prompt, aspect, resolution, duration string
		images                                      []imageData
	}{model, prompt, aspect, resolution, duration, images})
	return f.video, f.videoErr
}

func TestVideoPipeline(t *testing.T) {
	dir := t.TempDir()
	im := fixtureImage(t, color.NRGBA{R: 100, A: 255})
	input := filepath.Join(dir, "input.png")
	if err := os.WriteFile(input, im.Data, 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp4")
	f := &fakeVideoGenerator{video: []byte("video bytes")}
	o := options{prompt: "animate this", model: defaultVideoModel, video: true, input: input, output: out, aspect: "16:9", resolution: "720p", duration: "8s", timings: true}
	var log bytes.Buffer
	if err := run(context.Background(), o, f, &log); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || len(f.videoCalls) != 1 || len(f.videoCalls[0].images) != 1 || f.videoCalls[0].aspect != "16:9" || f.videoCalls[0].resolution != "720p" || f.videoCalls[0].duration != "8s" {
		t.Fatalf("calls: %+v", f.videoCalls)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, f.video) || !strings.Contains(log.String(), "Video saved") || !strings.Contains(log.String(), "Timing: API") {
		t.Fatalf("video output: %q, %v, %q", got, err, log.String())
	}
	for _, failure := range []error{errors.New("API unavailable"), context.Canceled} {
		failed := filepath.Join(dir, "failed.mp4")
		f := &fakeVideoGenerator{videoErr: failure}
		o.output = failed
		if err := run(context.Background(), o, f, io.Discard); err == nil {
			t.Fatal("expected failure")
		}
		if _, err := os.Stat(failed); !os.IsNotExist(err) {
			t.Fatal("failed video produced output")
		}
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
