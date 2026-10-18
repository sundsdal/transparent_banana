package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type imageAdvancedGenerator interface {
	generateImageAdvanced(context.Context, imageGenerationRequest) (imageGenerationResult, error)
	prepareImageInputs(context.Context, []string) ([]geminiPart, error)
}

type imageSession struct {
	Model    string            `json:"model"`
	Contents []json.RawMessage `json:"contents"`
}

func loadImageSession(path, model string) (imageSession, error) {
	session := imageSession{Model: model}
	if path == "" {
		return session, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return session, nil
	}
	if err != nil {
		return session, fmt.Errorf("read image session: %w", err)
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &kind); err == nil && kind.Kind == "video" {
		return session, errors.New("session file is a video session, not an image session")
	}
	if err := json.Unmarshal(data, &session); err != nil {
		return session, fmt.Errorf("decode image session: %w", err)
	}
	if session.Model != model {
		return session, fmt.Errorf("image session model %q does not match selected model %q", session.Model, model)
	}
	return session, nil
}

func saveImageSession(path string, session imageSession) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode image session: %w", err)
	}
	return atomicWriteFile(path, append(data, '\n'), 0o600)
}

func runAdvancedImage(ctx context.Context, o options, client imageAdvancedGenerator, out io.Writer) error {
	start := time.Now()
	var apiTime time.Duration
	session, err := loadImageSession(o.session, o.model)
	if err != nil {
		return err
	}
	var paths []string
	if o.input != "" {
		for _, raw := range strings.Split(o.input, ",") {
			path := strings.TrimSpace(raw)
			if path == "" {
				return errors.New("input paths cannot be empty")
			}
			paths = append(paths, path)
		}
	}
	imageCount := 0
	for _, path := range paths {
		if containsValue([]string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif"}, strings.ToLower(filepath.Ext(path))) {
			imageCount++
		}
	}
	if o.model == defaultModel && imageCount > 14 {
		return errors.New("Nano Banana 2.1 supports at most 14 input images")
	}
	parts, err := client.prepareImageInputs(ctx, paths)
	if err != nil {
		return err
	}
	requestOpts := imageRequestOptions{
		Aspect: o.aspect, Size: o.imageSize, Thinking: o.thinking,
		Search: o.search, IncludeThoughts: o.includeThoughts, Text: o.text,
	}
	call := func(contents []json.RawMessage) (imageGenerationResult, error) {
		apiStart := time.Now()
		result, err := client.generateImageAdvanced(ctx, imageGenerationRequest{Model: o.model, Contents: contents, Options: requestOpts})
		apiTime += time.Since(apiStart)
		return result, err
	}
	if o.transparent {
		prompt := o.prompt + ". On a pure solid white #FFFFFF background"
		if len(parts) > 0 {
			prompt = "This image contains " + o.prompt + ". Remove everything from the scene except " + o.prompt + ". Place the isolated object on a plain, pure white #FFFFFF background with no shadows, no reflections, and no other elements. Do not change the object itself in any way — preserve its exact colors, shape, size, and details."
		}
		whiteContent, err := imageContent(prompt, parts)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Step 1/3: Generating object on white background...")
		whiteResult, err := call([]json.RawMessage{whiteContent})
		if err != nil {
			return err
		}
		if len(whiteResult.Images) == 0 {
			return errors.New("white-background step did not produce an image")
		}
		blackContent, err := imageContent("Change the white background to a solid pure black #000000 background. Keep everything else exactly unchanged.", []geminiPart{imageInline(whiteResult.Images[0])})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Step 2/3: Editing background to black...")
		blackResult, err := call([]json.RawMessage{blackContent})
		if err != nil {
			return err
		}
		if len(blackResult.Images) == 0 {
			return errors.New("black-background step did not produce an image")
		}
		if o.intermediates {
			whitePath, blackPath := intermediatePaths(o.output)
			if err := saveImageAtomic(whitePath, whiteResult.Images[0]); err != nil {
				return err
			}
			if err := saveImageAtomic(blackPath, blackResult.Images[0]); err != nil {
				return err
			}
			fmt.Fprintf(out, "Saved intermediates: %s, %s\n", whitePath, blackPath)
		}
		fmt.Fprintln(out, "Step 3/3: Extracting alpha via difference matting...")
		matteImage, err := matte(whiteResult.Images[0], blackResult.Images[0])
		if err != nil {
			return err
		}
		result := imageGenerationResult{Images: []imageData{matteImage}, Segments: []imageSegment{{Kind: "image", ImageIndex: 1}}}
		for _, source := range []imageGenerationResult{whiteResult, blackResult} {
			thoughtOffset := len(result.ThoughtImages)
			result.ThoughtImages = append(result.ThoughtImages, source.ThoughtImages...)
			for _, segment := range source.Segments {
				if segment.Kind == "text" || segment.Thought && segment.Kind == "image" {
					if segment.Thought && segment.Kind == "image" {
						segment.ImageIndex += thoughtOffset
					}
					result.Segments = append(result.Segments, segment)
				}
			}
		}
		result.RawResponse, err = json.Marshal(map[string]json.RawMessage{"white": whiteResult.RawResponse, "black": blackResult.RawResponse})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := saveImageResult(o.output, result, o.responseJSON, o.includeThoughts); err != nil {
			return err
		}
		stem := strings.TrimSuffix(o.output, filepath.Ext(o.output))
		for _, source := range []struct {
			name   string
			result imageGenerationResult
		}{{"white", whiteResult}, {"black", blackResult}} {
			if len(source.result.Grounding) > 0 {
				if err := saveGrounding(stem+"-"+source.name+"-grounding.html", source.result); err != nil {
					return err
				}
				printGroundingSources(out, source.result)
			}
		}
		fmt.Fprintf(out, "Done! Image saved to: %s\n", o.output)
	} else {
		content, err := imageContent(o.prompt, parts)
		if err != nil {
			return err
		}
		contents := append(append([]json.RawMessage(nil), session.Contents...), content)
		if len(paths) > 0 {
			fmt.Fprintln(out, "Editing image...")
		} else {
			fmt.Fprintln(out, "Generating image...")
		}
		result, err := call(contents)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := saveImageResult(o.output, result, o.responseJSON, o.includeThoughts); err != nil {
			return err
		}
		if o.session != "" {
			if len(result.ModelContent) == 0 {
				return errors.New("Gemini did not return model content for session")
			}
			session.Contents = append(contents, result.ModelContent)
			if err := saveImageSession(o.session, session); err != nil {
				return err
			}
		}
		for i := range result.Images {
			fmt.Fprintf(out, "Image saved to: %s\n", numberedImagePath(o.output, i+1))
		}
		if o.text {
			for _, segment := range result.Segments {
				if segment.Kind == "text" && !segment.Thought {
					fmt.Fprintln(out, segment.Text)
				}
			}
		}
		if len(result.Images) > 1 || result.hasVisibleText() {
			fmt.Fprintf(out, "Result manifest: %s\n", strings.TrimSuffix(o.output, filepath.Ext(o.output))+".md")
		}
		printGroundingSources(out, result)
	}
	if o.timings {
		total := time.Since(start)
		fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
	}
	return nil
}

func printGroundingSources(out io.Writer, result imageGenerationResult) {
	for _, source := range groundingSources(result.Grounding) {
		fmt.Fprintf(out, "Source: %s\n", source.URL)
	}
}
