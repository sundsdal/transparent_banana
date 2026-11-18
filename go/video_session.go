package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type videoAdvancedGenerator interface {
	prepareOmniInputs(context.Context, []string) ([]omniInput, error)
	generateVideoAdvanced(context.Context, videoGenerationRequest) (videoGenerationResult, error)
}

type videoSession struct {
	Kind              string `json:"kind"`
	Version           int    `json:"version"`
	Model             string `json:"model"`
	LastInteractionID string `json:"last_interaction_id"`
	VertexProject     string `json:"vertex_project,omitempty"`
}

func loadVideoSession(path, model string) (videoSession, error) {
	session := videoSession{Kind: "video", Version: 1, Model: model}
	if path == "" {
		return session, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return session, nil
	}
	if err != nil {
		return session, fmt.Errorf("read video session: %w", err)
	}
	var loaded videoSession
	if err := json.Unmarshal(data, &loaded); err != nil {
		return session, fmt.Errorf("decode video session: %w", err)
	}
	if loaded.Kind != "video" || loaded.Version != 1 {
		return session, errors.New("session file is not a supported video session")
	}
	if !strings.HasPrefix(loaded.Model, "gemini-omni-") {
		return session, errors.New("video session has no valid Omni model")
	}
	if model != "" && loaded.Model != model {
		return session, fmt.Errorf("video session model %q does not match selected model %q", loaded.Model, model)
	}
	if strings.TrimSpace(loaded.LastInteractionID) == "" {
		return session, errors.New("video session has no interaction ID")
	}
	return loaded, nil
}

func saveVideoSession(path string, session videoSession) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode video session: %w", err)
	}
	return atomicWriteFile(path, append(data, '\n'), 0o600)
}

func videoInputPaths(o options) ([]string, int, error) {
	paths := make([]string, 0)
	frames := 0
	for _, path := range []string{o.firstFrame, o.lastFrame} {
		if path == "" {
			continue
		}
		mimeType, _, err := omniMediaMIME(path)
		if err != nil {
			return nil, 0, err
		}
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, 0, fmt.Errorf("frame input %q must be an image", path)
		}
		paths = append(paths, path)
		frames++
	}
	if o.input != "" {
		for _, raw := range strings.Split(o.input, ",") {
			path := strings.TrimSpace(raw)
			if path == "" {
				return nil, 0, errors.New("input paths cannot be empty")
			}
			paths = append(paths, path)
		}
	}
	return paths, frames, nil
}

func videoRolePrompt(prompt, task, previousID string, parts []omniInput, frameCount int) string {
	if frameCount == 0 && task != "edit" && task != "extend" && task != "reference_to_video" {
		return prompt
	}
	var sources, refs []string
	imageNumber, videoNumber, imageRef, videoRef := 0, 0, 0, 0
	videoSourceAssigned := false
	for i, part := range parts {
		switch part.Type {
		case "image":
			imageNumber++
			if i < frameCount {
				role := "FIRST_FRAME"
				if i == 1 {
					role = "LAST_FRAME"
				}
				sources = append(sources, fmt.Sprintf("<%s>@Image%d", role, imageNumber))
			} else {
				refs = append(refs, fmt.Sprintf("<IMAGE_REF_%d>@Image%d", imageRef, imageNumber))
				imageRef++
			}
		case "video":
			videoNumber++
			if (task == "edit" || task == "extend") && previousID == "" && !videoSourceAssigned {
				sources = append(sources, fmt.Sprintf("<VIDEO_0>@Video%d", videoNumber))
				videoSourceAssigned = true
			} else {
				refs = append(refs, fmt.Sprintf("<VIDEO_REF_%d>@Video%d", videoRef, videoNumber))
				videoRef++
			}
		}
	}
	var declarations []string
	if len(sources) > 0 {
		declarations = append(declarations, "[# Sources "+strings.Join(sources, " ")+"]")
	}
	if len(refs) > 0 {
		declarations = append(declarations, "[# References "+strings.Join(refs, " ")+"]")
	}
	if len(declarations) == 0 {
		return prompt
	}
	return strings.Join(declarations, " ") + " " + prompt
}

func omni12Prompt(prompt, task, previousID string, frameCount, imageCount int) string {
	switch frameCount {
	case 1:
		return "Use the first image as the starting frame. " + prompt
	case 2:
		return "Use the first image as the starting frame and the second image as the ending frame. " + prompt
	}
	if imageCount > 0 && (task == "edit" || task == "extend" || task == "reference_to_video" || previousID != "") {
		return "Use the images as references. " + prompt
	}
	return prompt
}

func runAdvancedVideo(ctx context.Context, o options, client videoAdvancedGenerator, out io.Writer) error {
	start := time.Now()
	sessionModel := o.model
	if !o.explicitModel && o.model == defaultVideoModel {
		sessionModel = ""
	}
	session, err := loadVideoSession(o.session, sessionModel)
	if err != nil {
		return err
	}
	if session.Model == "" {
		session.Model = o.model
	}
	if session.LastInteractionID != "" && session.VertexProject != o.vertexProject {
		return errors.New("video session belongs to a different API or Google Cloud project")
	}
	session.VertexProject = o.vertexProject
	o.model = session.Model
	if err := o.validateVideoModelOptions(); err != nil {
		return err
	}
	previousID := o.previousInteractionID
	if session.LastInteractionID != "" {
		if previousID != "" && previousID != session.LastInteractionID {
			return errors.New("--previous-interaction-id does not match the video session")
		}
		previousID = session.LastInteractionID
	}
	paths, frameCount, err := videoInputPaths(o)
	if err != nil {
		return err
	}
	imageCount, videoCount := 0, 0
	for _, path := range paths {
		mimeType, isVideo, err := omniMediaMIME(path)
		if err != nil {
			return err
		}
		if isVideo {
			videoCount++
		} else if strings.HasPrefix(mimeType, "image/") {
			imageCount++
		} else {
			return fmt.Errorf("Omni input %q must be an image or video", path)
		}
	}
	switch o.task {
	case "text_to_video":
		if len(paths) > 0 || previousID != "" {
			return errors.New("text_to_video requires a new text-only interaction")
		}
	case "image_to_video":
		if imageCount == 0 || videoCount > 0 {
			return errors.New("image_to_video requires image inputs and no video inputs")
		}
	case "reference_to_video":
		if len(paths) == 0 {
			return errors.New("reference_to_video requires media inputs")
		}
	case "edit", "extend":
		if videoCount == 0 && previousID == "" {
			return fmt.Errorf("%s requires a video input or previous interaction", o.task)
		}
	case "upscale":
		if (previousID == "" && (videoCount != 1 || len(paths) != 1)) ||
			(previousID != "" && len(paths) != 0) {
			return errors.New("upscaling requires one input video or a previous interaction")
		}
	}
	if o.model == defaultVideoModel {
		if err := o.validateOmni12Media(previousID, frameCount, imageCount, videoCount); err != nil {
			return err
		}
	}
	parts, err := client.prepareOmniInputs(ctx, paths)
	if err != nil {
		return err
	}
	prompt := o.prompt
	if o.model == defaultVideoModel {
		prompt = omni12Prompt(prompt, o.task, previousID, frameCount, imageCount)
	} else {
		prompt = videoRolePrompt(prompt, o.task, previousID, parts, frameCount)
	}
	request := videoGenerationRequest{
		Model: o.model, Prompt: prompt, Inputs: parts,
		Aspect: o.aspect, Resolution: o.resolution, Duration: o.duration,
		Task: o.task, PreviousInteractionID: previousID,
		Store:    o.session != "" || previousID != "" || o.stream || o.background,
		Delivery: o.delivery, GCSOutput: o.gcsOutput, GetInteractionID: o.getInteractionID,
		CancelInteractionID: o.cancelInteractionID,
		Seed: o.seed, Labels: o.labels, Background: o.background, CaptureResponse: o.responseJSON != "",
	}
	if o.vertexProject != "" && frameCount > 0 && request.Task == "" && frameCount == len(parts) {
		request.Task = "image_to_video"
	}
	if o.stream {
		request.StreamProgress = func(message string) { fmt.Fprintln(out, message) }
	}
	switch {
	case o.cancelInteractionID != "":
		fmt.Fprintln(out, "Cancelling video interaction...")
	case o.getInteractionID != "":
		fmt.Fprintln(out, "Waiting for video interaction...")
	case o.background:
		fmt.Fprintln(out, "Submitting video interaction...")
	default:
		fmt.Fprintln(out, "Generating video...")
	}
	apiStart := time.Now()
	result, err := client.generateVideoAdvanced(ctx, request)
	apiTime := time.Since(apiStart)
	if err != nil {
		return err
	}
	if result.VideoStream != nil {
		defer result.VideoStream.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.background || o.cancelInteractionID != "" {
		fmt.Fprintf(out, "Interaction ID: %s\n", result.InteractionID)
		if result.Status != "" {
			fmt.Fprintf(out, "Status: %s\n", result.Status)
		}
		if o.timings {
			total := time.Since(start)
			fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
		}
		return saveVideoResponse(o.responseJSON, result.RawResponse)
	}
	if result.VideoStream != nil {
		downloadStart := time.Now()
		err = saveVideoStream(ctx, o.output, result.VideoStream)
		apiTime += time.Since(downloadStart)
	} else {
		err = saveVideo(o.output, result.Video)
	}
	if err != nil {
		return err
	}
	if err := saveVideoResponse(o.responseJSON, result.RawResponse); err != nil {
		return err
	}
	if request.Store {
		if result.InteractionID == "" {
			return errors.New("Gemini returned no interaction ID; video saved but session was not advanced")
		}
		session.LastInteractionID = result.InteractionID
		if err := saveVideoSession(o.session, session); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Done! Video saved to: %s\n", o.output)
	if request.Store && !o.stream {
		fmt.Fprintf(out, "Interaction ID: %s\n", result.InteractionID)
	}
	for _, text := range result.Text {
		fmt.Fprintln(out, text)
	}
	if o.timings {
		total := time.Since(start)
		fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
	}
	return nil
}

func saveVideoResponse(path string, raw []byte) error {
	if path == "" {
		return nil
	}
	if len(raw) == 0 {
		return errors.New("Gemini returned no raw response to save")
	}
	if err := atomicWriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("save video response JSON: %w", err)
	}
	return nil
}
