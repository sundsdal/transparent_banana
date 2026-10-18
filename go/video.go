package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type omniInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

type omniVideoConfig struct {
	Task string `json:"task"`
}

type omniGenerationConfig struct {
	VideoConfig omniVideoConfig `json:"video_config"`
}

type omniVideoFormat struct {
	Type        string `json:"type"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Duration    string `json:"duration,omitempty"`
	Delivery    string `json:"delivery"`
}

type omniRequest struct {
	Model                 string                `json:"model"`
	Input                 any                   `json:"input"`
	ResponseFormat        omniVideoFormat       `json:"response_format"`
	GenerationConfig      *omniGenerationConfig `json:"generation_config,omitempty"`
	PreviousInteractionID string                `json:"previous_interaction_id,omitempty"`
	Background            bool                  `json:"background"`
	Stream                bool                  `json:"stream"`
	Store                 bool                  `json:"store"`
}

type omniResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type     string `json:"type"`
			MIMEType string `json:"mime_type"`
			Data     string `json:"data"`
			URI      string `json:"uri"`
			Text     string `json:"text"`
		} `json:"content"`
	} `json:"steps"`
}

var omniFileID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type videoGenerationRequest struct {
	Model, Prompt, Aspect, Resolution, Duration, Task string
	PreviousInteractionID                             string
	Store                                             bool
	Inputs                                            []omniInput
}

type videoGenerationResult struct {
	Video         []byte
	VideoStream   io.ReadCloser // The caller owns and must close URI download streams.
	InteractionID string
	RawResponse   []byte
}

// generateVideo preserves the legacy []byte interface; the advanced CLI streams URI downloads to disk.
func (c *geminiClient) generateVideo(ctx context.Context, model, prompt string, images []imageData, aspect, resolution, duration string) ([]byte, error) {
	parts := make([]omniInput, 0, len(images))
	for _, image := range images {
		if len(image.Data) == 0 || !strings.HasPrefix(strings.ToLower(image.MIMEType), "image/") {
			return nil, errors.New("video input must contain nonempty images")
		}
		parts = append(parts, omniInput{Type: "image", Data: base64.StdEncoding.EncodeToString(image.Data), MIMEType: image.MIMEType})
	}
	result, err := c.generateVideoAdvanced(ctx, videoGenerationRequest{
		Model: model, Prompt: prompt, Inputs: parts, Aspect: aspect, Resolution: resolution, Duration: duration,
	})
	if err != nil {
		return nil, err
	}
	if result.VideoStream != nil {
		defer result.VideoStream.Close()
		video, err := io.ReadAll(result.VideoStream)
		if err != nil {
			return nil, fmt.Errorf("read Gemini video: %w", err)
		}
		if len(video) == 0 {
			return nil, errors.New("Gemini returned an empty video file")
		}
		return video, nil
	}
	return result.Video, nil
}

func (c *geminiClient) generateVideoAdvanced(ctx context.Context, request videoGenerationRequest) (videoGenerationResult, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return videoGenerationResult{}, errors.New("Gemini API key is required")
	}
	if !strings.HasPrefix(request.Model, "gemini-omni-") {
		return videoGenerationResult{}, errors.New("an Omni model is required for video")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return videoGenerationResult{}, errors.New("a prompt is required for video")
	}
	var input any = request.Prompt
	if len(request.Inputs) > 0 {
		parts := make([]omniInput, 0, len(request.Inputs)+1)
		for _, part := range request.Inputs {
			if part.Type != "image" && part.Type != "video" || (part.Data == "") == (part.URI == "") {
				return videoGenerationResult{}, errors.New("video input must contain images or videos with exactly one data source")
			}
			parts = append(parts, part)
		}
		parts = append(parts, omniInput{Type: "text", Text: request.Prompt})
		input = parts
	}
	omniReq := omniRequest{
		Model: request.Model, Input: input,
		ResponseFormat:        omniVideoFormat{Type: "video", AspectRatio: request.Aspect, Resolution: request.Resolution, Duration: request.Duration, Delivery: "uri"},
		PreviousInteractionID: request.PreviousInteractionID, Store: request.Store,
	}
	if request.Task != "" {
		omniReq.GenerationConfig = &omniGenerationConfig{VideoConfig: omniVideoConfig{Task: request.Task}}
	}
	payload, err := json.Marshal(omniReq)
	if err != nil {
		return videoGenerationResult{}, fmt.Errorf("encode Gemini video request: %w", err)
	}
	response, err := c.videoRequest(ctx, http.MethodPost, "/interactions", bytes.NewReader(payload), "application/json", false)
	if err != nil {
		return videoGenerationResult{}, err
	}
	defer response.Body.Close()
	if err := c.videoStatusError(response); err != nil {
		return videoGenerationResult{}, err
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return videoGenerationResult{}, fmt.Errorf("read Gemini video response: %w", err)
	}
	var result omniResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return videoGenerationResult{}, fmt.Errorf("decode Gemini video response: %w", err)
	}
	if result.Status != "completed" {
		detail := c.boundedDiagnostic(result.Error.Message)
		if detail != "" {
			return videoGenerationResult{}, fmt.Errorf("Gemini video interaction %s: %s", c.boundedDiagnostic(result.Status), detail)
		}
		return videoGenerationResult{}, fmt.Errorf("Gemini video interaction %s", c.boundedDiagnostic(result.Status))
	}
	for _, step := range result.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, part := range step.Content {
			if part.Type != "video" || strings.ToLower(strings.TrimSpace(strings.Split(part.MIMEType, ";")[0])) != "video/mp4" {
				continue
			}
			if part.URI != "" {
				stream, err := c.openVideoFile(ctx, part.URI)
				if err != nil {
					return videoGenerationResult{}, err
				}
				return videoGenerationResult{VideoStream: stream, InteractionID: result.ID, RawResponse: raw}, nil
			}
			if part.Data != "" {
				data, err := base64.StdEncoding.DecodeString(part.Data)
				if err != nil {
					return videoGenerationResult{}, fmt.Errorf("decode Gemini video data: %w", err)
				}
				if len(data) > 0 {
					return videoGenerationResult{Video: data, InteractionID: result.ID, RawResponse: raw}, nil
				}
			}
		}
	}
	var details []string
	for _, step := range result.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, part := range step.Content {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				details = append(details, part.Text)
			}
		}
	}
	if len(details) > 0 {
		return videoGenerationResult{}, fmt.Errorf("Gemini response did not contain an MP4 video: %s", c.boundedDiagnostic(strings.Join(details, "; ")))
	}
	return videoGenerationResult{}, errors.New("Gemini response did not contain an MP4 video")
}

func (c *geminiClient) videoRequest(ctx context.Context, method, path string, body io.Reader, contentType string, allowDownloadRedirects bool) (*http.Response, error) {
	baseURL := c.baseURL
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, fmt.Errorf("create Gemini video request: %w", err)
	}
	request.Header.Set("x-goog-api-key", c.apiKey)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	requestClient := *client
	requestClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !allowDownloadRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("too many video download redirects")
		}
		if next.URL.Host != via[0].URL.Host || next.URL.Scheme != via[0].URL.Scheme {
			if next.URL.Scheme != "https" {
				return errors.New("video download redirected to a non-HTTPS origin")
			}
			next.Header.Del("x-goog-api-key")
		}
		return nil
	}
	response, err := requestClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("send Gemini video request: %w", ctx.Err())
		}
		return nil, errors.New("send Gemini video request failed")
	}
	return response, nil
}

func (c *geminiClient) videoStatusError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		return fmt.Errorf("Gemini video API returned %s", response.Status)
	}
	var apiErr geminiAPIError
	message := ""
	if json.Unmarshal(data, &apiErr) == nil {
		message = apiErr.Error.Message
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("Gemini video API returned %s: %s", response.Status, c.boundedDiagnostic(message))
}

func (c *geminiClient) openVideoFile(ctx context.Context, uri string) (io.ReadCloser, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, errors.New("Gemini returned an invalid video file URI")
	}
	path := parsed.Path
	if !strings.HasSuffix(path, ":download") {
		return nil, errors.New("Gemini returned an invalid video file URI")
	}
	path = strings.TrimSuffix(path, ":download")
	const marker = "/files/"
	index := strings.LastIndex(path, marker)
	if index < 0 || path[:index] != "/v1beta" {
		return nil, errors.New("Gemini returned an invalid video file URI")
	}
	id := path[index+len(marker):]
	if !omniFileID.MatchString(id) {
		return nil, errors.New("Gemini returned an invalid video file URI")
	}
	filePath := "/files/" + url.PathEscape(id)
	for {
		response, err := c.videoRequest(ctx, http.MethodGet, filePath, nil, "", false)
		if err != nil {
			return nil, err
		}
		if err := c.videoStatusError(response); err != nil {
			response.Body.Close()
			return nil, err
		}
		var file struct {
			State string `json:"state"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&file)
		response.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode Gemini video file status: %w", decodeErr)
		}
		switch file.State {
		case "ACTIVE":
			response, err := c.videoRequest(ctx, http.MethodGet, filePath+":download?alt=media", nil, "", true)
			if err != nil {
				return nil, err
			}
			if err := c.videoStatusError(response); err != nil {
				response.Body.Close()
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				response.Body.Close()
				return nil, err
			}
			return response.Body, nil
		case "PROCESSING":
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, ctx.Err()
			case <-timer.C:
			}
		case "FAILED":
			return nil, fmt.Errorf("Gemini video file failed: %s", c.boundedDiagnostic(file.Error.Message))
		default:
			return nil, fmt.Errorf("Gemini video file has unexpected state: %s", c.boundedDiagnostic(file.State))
		}
	}
}

func saveVideoStream(ctx context.Context, path string, source io.Reader) error {
	if !strings.EqualFold(filepath.Ext(path), ".mp4") {
		return errors.New("video output must have a .mp4 extension")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".nanobanana-video-*")
	if err != nil {
		return fmt.Errorf("create video output: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	size, err := io.Copy(file, source)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("copy video output: %w", err)
	}
	if size == 0 {
		return errors.New("Gemini returned an empty video file")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close video output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save video output: %w", err)
	}
	return nil
}

func saveVideo(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New("cannot save an empty video")
	}
	if !strings.EqualFold(filepath.Ext(path), ".mp4") {
		return errors.New("video output must have a .mp4 extension")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".nanobanana-video-*")
	if err != nil {
		return fmt.Errorf("create video output: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write video output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close video output: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save video output: %w", err)
	}
	return nil
}
