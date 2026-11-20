package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var bundledAPIKey string

const defaultModel = "gemini-nano-banana-2.1"
const defaultVideoModel = "gemini-omni-1.2-flash"
const legacyVideoModel = "gemini-omni-1.1-flash"

var models = map[string]string{
	"flash":      defaultModel,
	"flash-lite": "gemini-3.1-flash-lite-image",
	"pro":        "gemini-3-pro-image",
	"flash-2":    "gemini-2.5-flash-image",
	"omni":       defaultVideoModel,
	"omni-1.1":   legacyVideoModel,
}

type options struct {
	prompt, input, output, model, aspect, resolution, duration string
	imageSize, thinking, search, session, responseJSON         string
	task, firstFrame, lastFrame, previousInteractionID         string
	vertexProject, gcsOutput, delivery, getInteractionID       string
	vertexTraffic, cancelInteractionID                         string
	seed                                                       *int32
	labels                                                     map[string]string
	transparent, intermediates, timings, help, video, text     bool
	includeThoughts, stream, background, explicitModel         bool
}

// validate resolves the model, mode, prompt, and defaults from parsed flags and
// rejects option combinations before any network request.
func (o *options) validate(positional []string, explicitOutput bool) error {
	if id, ok := models[o.model]; ok {
		o.model = id
	}
	if o.video || o.vertexProject != "" || o.getInteractionID != "" || o.cancelInteractionID != "" {
		if o.explicitModel && !strings.HasPrefix(o.model, "gemini-omni-") {
			return errors.New("--video requires an Omni model")
		}
		if !o.explicitModel {
			o.model = defaultVideoModel
		}
	}
	o.video = strings.HasPrefix(o.model, "gemini-omni-")
	if o.video && (o.imageSize != "" || o.thinking != "" || o.includeThoughts || o.search != "" || o.text) {
		return errors.New("image options cannot be used with an Omni video model")
	}
	if strings.ContainsAny(o.model, "/?# \t\n") {
		return errors.New("invalid model ID")
	}
	if len(positional) == 1 && strings.TrimSpace(positional[0]) != "" {
		o.prompt = positional[0]
	} else if len(positional) != 0 || !o.video || (o.task != "upscale" && o.getInteractionID == "" && o.cancelInteractionID == "") {
		return errors.New("provide one quoted, nonempty prompt")
	}
	validAspects := []string{"1:1", "1:4", "1:8", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "21:9"}
	if o.video {
		if explicitOutput && (o.background || o.cancelInteractionID != "") {
			return errors.New("--output applies when saving a video")
		}
		validAspects = []string{"9:16", "16:9"}
	}
	if o.aspect != "" && !containsValue(validAspects, o.aspect) {
		return fmt.Errorf("unsupported aspect ratio: %s", o.aspect)
	}
	if collision, err := responseJSONInputCollision(*o); err != nil {
		return err
	} else if collision {
		return errors.New("response JSON path must differ from media inputs")
	}
	if o.video {
		if !explicitOutput {
			o.output = "output.mp4"
		}
		err := o.validateVideoOptions()
		return err
	}
	if o.task != "" || o.firstFrame != "" || o.lastFrame != "" || o.previousInteractionID != "" ||
		o.delivery != "" || o.background || o.stream || o.seed != nil || len(o.labels) != 0 || o.gcsOutput != "" || o.vertexTraffic != "" {
		return errors.New("video options require an Omni video model")
	}
	if o.resolution != "" {
		if o.imageSize != "" && o.imageSize != o.resolution {
			return errors.New("--resolution and --image-size disagree")
		}
		o.imageSize = o.resolution
	}
	if o.imageSize != "" && !containsValue([]string{"1K", "2K", "4K"}, o.imageSize) {
		return errors.New("image size must be 1K, 2K, or 4K")
	}
	if o.thinking != "" && !containsValue([]string{"minimal", "medium", "high"}, o.thinking) {
		return errors.New("thinking level must be minimal, medium, or high")
	}
	if o.search != "" && !containsValue([]string{"web", "image", "both", "none"}, o.search) {
		return errors.New("search mode must be web, image, both, or none")
	}
	if o.session != "" && o.transparent {
		return errors.New("--session cannot be combined with --transparent")
	}
	if o.model != defaultModel && (o.thinking != "" || o.includeThoughts || o.search != "") {
		return errors.New("thinking and search options require the default Nano Banana 2.1 model")
	}
	if o.duration != "" {
		return errors.New("--duration requires video generation")
	}
	output := strings.ToLower(o.output)
	if !strings.HasSuffix(output, ".png") && !strings.HasSuffix(output, ".jpg") && !strings.HasSuffix(output, ".jpeg") {
		return errors.New("output must have a .png, .jpg, or .jpeg extension")
	}
	if o.transparent && !strings.HasSuffix(output, ".png") {
		return errors.New("transparent output requires a .png file")
	}
	if collision, err := imageArtifactCollision(*o, o.session); err != nil {
		return err
	} else if collision {
		return errors.New("session path must differ from image output artifacts")
	}
	if collision, err := imageArtifactCollision(*o, o.responseJSON); err != nil {
		return err
	} else if collision {
		return errors.New("response JSON path must differ from image output artifacts")
	}
	if collision, err := sameLocalPath(o.responseJSON, o.session); err != nil {
		return err
	} else if collision {
		return errors.New("response JSON path must differ from session")
	}
	return nil
}

func responseJSONInputCollision(o options) (bool, error) {
	if o.responseJSON == "" {
		return false, nil
	}
	inputs := []string{o.firstFrame, o.lastFrame}
	if o.input != "" {
		for _, raw := range strings.Split(o.input, ",") {
			inputs = append(inputs, strings.TrimSpace(raw))
		}
	}
	return pathsCollide(o.responseJSON, inputs...)
}

func imageArtifactCollision(o options, path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	stem := strings.TrimSuffix(o.output, filepath.Ext(o.output))
	artifacts := []string{o.output, stem + ".md"}
	if o.transparent {
		artifacts = append(artifacts, stem+"-white-grounding.html", stem+"-black-grounding.html")
		if o.intermediates {
			white, black := intermediatePaths(o.output)
			artifacts = append(artifacts, white, black)
		}
	} else {
		artifacts = append(artifacts, stem+"-grounding.html")
	}
	if o.includeThoughts {
		artifacts = append(artifacts, stem+"-thoughts.md")
	}
	if collision, err := pathsCollide(path, artifacts...); err != nil || collision {
		return collision, err
	}
	if !o.transparent {
		if collision, err := numberedArtifactCollision(path, stem, filepath.Ext(o.output), 2); err != nil || collision {
			return collision, err
		}
	}
	if o.includeThoughts {
		return numberedArtifactCollision(path, stem+"-thought", ".png", 1)
	}
	return false, nil
}

func numberedArtifactCollision(path, stem, ext string, first int) (bool, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", path, err)
	}
	absolutePrefix, err := filepath.Abs(stem + "-")
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", stem, err)
	}
	number, found := strings.CutPrefix(absolutePath, absolutePrefix)
	if !found || !strings.HasSuffix(number, ext) {
		return false, nil
	}
	number = strings.TrimSuffix(number, ext)
	if number == "" {
		return false, nil
	}
	index, err := strconv.Atoi(number)
	return err == nil && index >= first && strconv.Itoa(index) == number, nil
}

func sameLocalPath(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	left, err := filepath.Abs(a)
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", a, err)
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", b, err)
	}
	return left == right, nil
}

func pathsCollide(path string, others ...string) (bool, error) {
	for _, other := range others {
		collision, err := sameLocalPath(path, other)
		if err != nil || collision {
			return collision, err
		}
	}
	return false, nil
}

func containsValue(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

type generator interface {
	generate(context.Context, string, string, []imageData, string) (imageData, error)
}

type videoGenerator interface {
	generateVideo(context.Context, string, string, []imageData, string, string, string) ([]byte, error)
}

func run(ctx context.Context, o options, client generator, out io.Writer) error {
	if o.video {
		if advanced, ok := client.(videoAdvancedGenerator); ok {
			return runAdvancedVideo(ctx, o, advanced, out)
		}
	}
	if !o.video {
		if advanced, ok := client.(imageAdvancedGenerator); ok {
			return runAdvancedImage(ctx, o, advanced, out)
		}
	}
	start := time.Now()
	var apiTime time.Duration
	var images []imageData
	if o.input != "" {
		for _, path := range strings.Split(o.input, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				return errors.New("input paths cannot be empty")
			}
			img, err := loadImage(path)
			if err != nil {
				return err
			}
			images = append(images, img)
		}
	}
	if o.video {
		videoClient, ok := client.(videoGenerator)
		if !ok {
			return errors.New("video generation is unavailable")
		}
		fmt.Fprintln(out, "Generating video...")
		apiStart := time.Now()
		video, err := videoClient.generateVideo(ctx, o.model, o.prompt, images, o.aspect, o.resolution, o.duration)
		apiTime += time.Since(apiStart)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := saveVideo(o.output, video); err != nil {
			return err
		}
		fmt.Fprintf(out, "Done! Video saved to: %s\n", o.output)
		if o.timings {
			total := time.Since(start)
			fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
		}
		return nil
	}
	call := func(prompt string, refs []imageData, aspect string) (imageData, error) {
		t := time.Now()
		img, err := client.generate(ctx, o.model, prompt, refs, aspect)
		apiTime += time.Since(t)
		return img, err
	}
	var result imageData
	if o.transparent {
		prompt := o.prompt + ". On a pure solid white #FFFFFF background"
		if len(images) > 0 {
			prompt = "This image contains " + o.prompt + ". Remove everything from the scene except " + o.prompt + ". Place the isolated object on a plain, pure white #FFFFFF background with no shadows, no reflections, and no other elements. Do not change the object itself in any way — preserve its exact colors, shape, size, and details."
		}
		fmt.Fprintln(out, "Step 1/3: Generating object on white background...")
		white, err := call(prompt, images, o.aspect)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Step 2/3: Editing background to black...")
		black, err := call("Change the white background to a solid pure black #000000 background. Keep everything else exactly unchanged.", []imageData{white}, o.aspect)
		if err != nil {
			return err
		}
		if o.intermediates {
			wp, bp := intermediatePaths(o.output)
			if err := saveImage(wp, white); err != nil {
				return err
			}
			if err := saveImage(bp, black); err != nil {
				return err
			}
			fmt.Fprintf(out, "Saved intermediates: %s, %s\n", wp, bp)
		}
		fmt.Fprintln(out, "Step 3/3: Extracting alpha via difference matting...")
		result, err = matte(white, black)
		if err != nil {
			return err
		}
	} else {
		if len(images) > 0 {
			fmt.Fprintln(out, "Editing image...")
		} else {
			fmt.Fprintln(out, "Generating image...")
		}
		var err error
		result, err = call(o.prompt, images, o.aspect)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := saveImage(o.output, result); err != nil {
		return err
	}
	fmt.Fprintf(out, "Done! Image saved to: %s\n", o.output)
	if o.timings {
		total := time.Since(start)
		fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
	}
	return nil
}

func resolveAPIKey() string {
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		return key
	}
	if bundledAPIKey != "" {
		return bundledAPIKey
	}
	return os.Getenv("NANOBANANA_BUNDLE_KEY")
}
