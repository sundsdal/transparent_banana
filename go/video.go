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
	VideoConfig *omniVideoConfig `json:"video_config,omitempty"`
	Seed        *int32           `json:"seed,omitempty"`
}

type omniVideoFormat struct {
	Type        string `json:"type"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Duration    string `json:"duration,omitempty"`
	Delivery    string `json:"delivery"`
	GCSURI      string `json:"gcs_uri,omitempty"`
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
	Labels                map[string]string     `json:"labels,omitempty"`
}

type omniStep struct {
	Type    string      `json:"type"`
	Content []omniInput `json:"content"`
}

type omniError struct {
	Message string `json:"message"`
}

type omniResponse struct {
	ID          string      `json:"id"`
	Status      string      `json:"status"`
	Error       omniError   `json:"error"`
	Errors      []omniError `json:"errors"`
	Steps       []omniStep  `json:"steps"`
	OutputVideo omniInput   `json:"output_video"`
}

var omniFileID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type videoGenerationRequest struct {
	Model, Prompt, Aspect, Resolution, Duration, Task string
	PreviousInteractionID                             string
	Delivery, GCSOutput, GetInteractionID             string
	CancelInteractionID                               string
	Seed                                              *int32
	Labels                                            map[string]string
	Store, Background, CaptureResponse                bool
	StreamProgress                                    func(string)
	Inputs                                            []omniInput
}

type videoGenerationResult struct {
	Video         []byte
	VideoStream   io.ReadCloser // The caller owns and must close URI download streams.
	InteractionID string
	RawResponse   []byte
	Text          []string
	Status        string
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
	if c.vertexProject == "" && strings.TrimSpace(c.apiKey) == "" {
		return videoGenerationResult{}, errors.New("Gemini API key is required")
	}
	if request.GetInteractionID != "" {
		return c.getVideoInteraction(ctx, request)
	}
	if request.CancelInteractionID != "" {
		return c.cancelVideoInteraction(ctx, request.CancelInteractionID)
	}
	if !strings.HasPrefix(request.Model, "gemini-omni-") {
		return videoGenerationResult{}, errors.New("an Omni model is required for video")
	}
	if strings.TrimSpace(request.Prompt) == "" && request.Task != "upscale" {
		return videoGenerationResult{}, errors.New("a prompt is required for video")
	}
	parts := make([]omniInput, 0, len(request.Inputs)+1)
	for _, part := range request.Inputs {
		if (part.Type != "image" && part.Type != "video") || (part.Data == "") == (part.URI == "") {
			return videoGenerationResult{}, errors.New("video input must contain images or videos with exactly one data source")
		}
		parts = append(parts, part)
	}
	prompt := request.Prompt
	if request.Task == "upscale" && request.PreviousInteractionID != "" {
		prompt = "Upsample video."
	}
	var input any = prompt
	if len(parts) > 0 || c.vertexProject != "" {
		if prompt != "" {
			parts = append(parts, omniInput{Type: "text", Text: prompt})
		}
		input = parts
		if c.vertexProject != "" && request.Store && !request.Background {
			input = []omniStep{{Type: "user_input", Content: parts}}
		}
	}
	delivery := request.Delivery
	if delivery == "" {
		delivery = "uri"
		if c.vertexProject != "" && request.GCSOutput == "" {
			delivery = "inline"
		}
	}
	omniReq := omniRequest{
		Model: request.Model, Input: input,
		ResponseFormat:        omniVideoFormat{Type: "video", AspectRatio: request.Aspect, Resolution: request.Resolution, Duration: request.Duration, Delivery: delivery, GCSURI: request.GCSOutput},
		PreviousInteractionID: request.PreviousInteractionID, Store: request.Store,
		Background: request.Background, Stream: request.StreamProgress != nil, Labels: request.Labels,
	}
	if request.Task != "" || request.Seed != nil {
		omniReq.GenerationConfig = &omniGenerationConfig{Seed: request.Seed}
		if request.Task != "" {
			omniReq.GenerationConfig.VideoConfig = &omniVideoConfig{Task: request.Task}
		}
	}
	var body any = omniReq
	if c.vertexProject != "" {
		body = struct {
			omniRequest
			ResponseFormat []omniVideoFormat `json:"response_format"`
		}{omniReq, []omniVideoFormat{omniReq.ResponseFormat}}
	}
	payload, err := json.Marshal(body)
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
	var interaction omniResponse
	var raw []byte
	if omniReq.Stream {
		interaction, raw, err = c.readVideoEvents(response.Body, request.StreamProgress, request.CaptureResponse)
	} else {
		interaction, raw, err = readVideoInteraction(response.Body)
	}
	if err != nil {
		return videoGenerationResult{}, err
	}
	if request.Background && (interactionPending(interaction.Status) || interaction.Status == "completed") {
		if interaction.ID == "" {
			return videoGenerationResult{}, errors.New("Gemini returned no background interaction ID")
		}
		return videoGenerationResult{InteractionID: interaction.ID, Status: interaction.Status, RawResponse: raw}, nil
	}
	return c.videoResult(ctx, interaction, raw)
}

func readVideoInteraction(body io.Reader) (omniResponse, []byte, error) {
	var result omniResponse
	raw, err := io.ReadAll(body)
	if err != nil {
		return result, nil, fmt.Errorf("read Gemini video response: %w", err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, nil, fmt.Errorf("decode Gemini video response: %w", err)
	}
	return result, raw, nil
}

func (c *geminiClient) videoResult(ctx context.Context, result omniResponse, raw []byte) (videoGenerationResult, error) {
	if result.Status != "completed" {
		detail := result.Error.Message
		if detail == "" && len(result.Errors) > 0 {
			detail = result.Errors[0].Message
		}
		detail = c.boundedDiagnostic(detail)
		if detail != "" {
			return videoGenerationResult{}, fmt.Errorf("Gemini video interaction %s: %s", c.boundedDiagnostic(result.Status), detail)
		}
		return videoGenerationResult{}, fmt.Errorf("Gemini video interaction %s", c.boundedDiagnostic(result.Status))
	}
	output := videoGenerationResult{InteractionID: result.ID, RawResponse: raw}
	video := result.OutputVideo
	for _, step := range result.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, part := range step.Content {
			if part.Type == "video" && isMP4(part.MIMEType) && (part.URI != "" || part.Data != "") &&
				(!isMP4(video.MIMEType) || (video.URI == "" && video.Data == "")) {
				video = part
			}
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				output.Text = append(output.Text, c.redact(part.Text))
			}
		}
	}
	if isMP4(video.MIMEType) {
		if video.URI != "" {
			var err error
			if c.vertexProject != "" {
				output.VideoStream, err = c.openCloudVideo(ctx, video.URI)
			} else {
				output.VideoStream, err = c.openVideoFile(ctx, video.URI)
			}
			return output, err
		}
		if video.Data != "" {
			var err error
			output.Video, err = base64.StdEncoding.DecodeString(video.Data)
			if err != nil {
				return videoGenerationResult{}, fmt.Errorf("decode Gemini video data: %w", err)
			}
			if len(output.Video) != 0 {
				return output, nil
			}
		}
	}
	if len(output.Text) > 0 {
		return videoGenerationResult{}, fmt.Errorf("Gemini response did not contain an MP4 video: %s", c.boundedDiagnostic(strings.Join(output.Text, "; ")))
	}
	return videoGenerationResult{}, errors.New("Gemini response did not contain an MP4 video")
}

func isMP4(mimeType string) bool {
	mediaType, _, _ := strings.Cut(mimeType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "video/mp4")
}

func (c *geminiClient) videoRequest(ctx context.Context, method, path string, body io.Reader, contentType string, allowDownloadRedirects bool) (*http.Response, error) {
	baseURL := c.baseURL
	if baseURL == "" {
		if c.vertexProject == "" {
			baseURL = defaultGeminiBaseURL
		} else {
			baseURL = "https://aiplatform.googleapis.com/v1beta1/projects/" + url.PathEscape(c.vertexProject) + "/locations/global"
		}
	}
	return c.sendVideoRequest(ctx, method, strings.TrimRight(baseURL, "/")+path, body, contentType, allowDownloadRedirects)
}

func (c *geminiClient) sendVideoRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string, allowDownloadRedirects bool) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create Gemini video request: %w", err)
	}
	if c.vertexProject == "" {
		request.Header.Set("x-goog-api-key", c.apiKey)
		if strings.Contains(request.URL.Path, "/interactions") {
			request.Header.Set("Api-Revision", "2026-05-20")
		}
	} else {
		request.Header.Set("Authorization", "Bearer "+c.vertexToken)
		request.Header.Set("x-goog-user-project", c.vertexProject)
		if c.vertexTraffic != "" && method == http.MethodPost {
			request.Header.Set("X-Vertex-AI-LLM-Request-Type", c.vertexTraffic)
		}
	}
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
			next.Header.Del("Authorization")
			next.Header.Del("x-goog-user-project")
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
	if strings.HasPrefix(path, "files/") {
		path = "/v1beta/" + path
	} else if strings.HasPrefix(path, "/files/") {
		path = "/v1beta" + path
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
