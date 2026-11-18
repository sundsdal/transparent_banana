package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func (o *options) validateVideoOptions() error {
	if o.transparent || o.intermediates {
		return errors.New("transparent output and intermediates are image-only options")
	}
	if !strings.EqualFold(filepath.Ext(o.output), ".mp4") {
		return errors.New("video output must have a .mp4 extension")
	}
	if o.task != "" && !containsValue([]string{"text_to_video", "image_to_video", "reference_to_video", "edit", "extend", "upscale"}, o.task) {
		return fmt.Errorf("unsupported video task: %s", o.task)
	}
	if o.lastFrame != "" && o.firstFrame == "" {
		return errors.New("--last-frame requires --first-frame")
	}
	if o.firstFrame != "" && (o.task == "text_to_video" || o.task == "edit" || o.task == "extend" || o.task == "upscale") {
		return errors.New("frame inputs require an image or reference video task")
	}
	if o.task == "text_to_video" && (o.input != "" || o.previousInteractionID != "") {
		return errors.New("text_to_video requires a new text-only interaction")
	}
	for _, id := range []string{o.previousInteractionID, o.getInteractionID, o.cancelInteractionID} {
		if id != "" && !omniFileID.MatchString(id) {
			return errors.New("invalid Omni interaction ID")
		}
	}
	if o.vertexProject != "" && !vertexProjectID.MatchString(o.vertexProject) {
		return errors.New("--vertex-project requires a Google Cloud project ID or number")
	}
	if o.vertexProject == "" && (o.gcsOutput != "" || o.seed != nil || len(o.labels) != 0 || o.task == "upscale" || o.vertexTraffic != "") {
		return errors.New("--gcs-output, --seed, --label, --vertex-traffic, and upscaling require --vertex-project")
	}
	if o.vertexTraffic != "" && o.vertexTraffic != "shared" && o.vertexTraffic != "dedicated" {
		return errors.New("--vertex-traffic must be shared or dedicated")
	}
	if o.gcsOutput != "" {
		if _, _, err := splitGCSURI(o.gcsOutput); err != nil {
			return err
		}
		if o.delivery == "inline" {
			return errors.New("--gcs-output requires URI delivery")
		}
	}
	if o.delivery != "" && o.delivery != "inline" && o.delivery != "uri" {
		return errors.New("--delivery must be inline or uri")
	}
	if o.vertexProject != "" && o.delivery == "uri" && o.gcsOutput == "" {
		return errors.New("URI delivery on Google Cloud requires --gcs-output")
	}
	if o.background && (o.stream || o.session != "" || o.getInteractionID != "" || o.cancelInteractionID != "") {
		return errors.New("--background cannot be combined with --stream, --session, or interaction retrieval and cancellation")
	}
	if o.getInteractionID != "" && o.cancelInteractionID != "" {
		return errors.New("choose either --get-interaction-id or --cancel-interaction-id")
	}
	if (o.getInteractionID != "" || o.cancelInteractionID != "") && (o.prompt != "" || o.input != "" || o.firstFrame != "" ||
		o.task != "" || o.session != "" || o.previousInteractionID != "" || o.vertexTraffic != "" ||
		o.explicitModel || o.delivery != "" || o.gcsOutput != "" || o.seed != nil || len(o.labels) != 0 ||
		o.aspect != "" || o.resolution != "" || o.duration != "") {
		return errors.New("retrieving or cancelling an interaction does not accept generation options")
	}
	if o.cancelInteractionID != "" && o.stream {
		return errors.New("--stream cannot be used when cancelling an interaction")
	}
	if o.resolution != "" && !containsValue([]string{"360p", "720p", "1080p", "4k"}, o.resolution) {
		return fmt.Errorf("unsupported video resolution: %s", o.resolution)
	}
	if o.task == "upscale" {
		if o.prompt != "" || o.aspect != "" || o.duration != "" {
			return errors.New("upscaling uses a source video and a target resolution")
		}
		if !containsValue([]string{"720p", "1080p", "4k"}, o.resolution) {
			return errors.New("upscaling requires --resolution 720p, 1080p, or 4k")
		}
	}
	if err := o.validateVideoModelOptions(); err != nil {
		return err
	}
	paths, _, err := videoInputPaths(*o)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "gs://") {
			if o.vertexProject == "" {
				return errors.New("Cloud Storage inputs require --vertex-project")
			}
			if _, object, err := splitGCSURI(path); err != nil || object == "" {
				return fmt.Errorf("invalid Cloud Storage input URI %q", path)
			}
			continue
		}
		if collision, err := pathsCollide(path, o.output, o.session); err != nil {
			return err
		} else if collision {
			return errors.New("video output and session paths must differ from media inputs")
		}
	}
	if collision, err := sameLocalPath(o.session, o.output); err != nil {
		return err
	} else if collision {
		return errors.New("session path must differ from video output")
	}
	if collision, err := pathsCollide(o.responseJSON, o.output, o.session); err != nil {
		return err
	} else if collision {
		return errors.New("response JSON path must differ from video output and session")
	}
	return nil
}

func (o *options) validateVideoModelOptions() error {
	if o.model != defaultVideoModel && (o.task == "upscale" || o.seed != nil || len(o.labels) != 0) {
		return errors.New("upscaling, --seed, and --label require Gemini Omni 1.2 Flash")
	}
	if o.duration == "" {
		return nil
	}
	maxDuration := 10
	if o.model == defaultVideoModel && o.lastFrame == "" && o.task != "extend" {
		maxDuration = 15
	}
	seconds := strings.TrimSuffix(o.duration, "s")
	n, err := strconv.Atoi(seconds)
	if err != nil || strings.Trim(seconds, "0123456789") != "" || n < 3 || n > maxDuration {
		return fmt.Errorf("video duration must be 3 to %d whole seconds for the selected model and task", maxDuration)
	}
	o.duration = strconv.Itoa(n) + "s"
	return nil
}

func (o options) validateOmni12Media(previousID string, frameCount, imageCount, videoCount int) error {
	if o.task == "edit" || o.task == "extend" {
		if videoCount > 1 || (videoCount == 1 && previousID != "") {
			return errors.New("editing and extension accept one source video or a previous interaction, with optional reference images")
		}
		if o.vertexProject != "" && imageCount > 2 {
			return errors.New("editing and extension on Google Cloud accept up to two reference images")
		}
	}
	if o.vertexProject == "" {
		if imageCount-frameCount > 6 {
			return errors.New("Gemini Omni accepts up to six image references")
		}
		if videoCount > 3 {
			return errors.New("Gemini Omni accepts up to three video references")
		}
		seconds, _ := strconv.Atoi(strings.TrimSuffix(o.duration, "s"))
		if seconds > 10 && videoCount > 1 {
			return errors.New("videos longer than 10 seconds accept one video reference")
		}
	}
	return nil
}
