package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const defaultTTSModel = "gemini-3.8-flash-tts"
const defaultTTSVoice = "Kore"

var ttsModels = map[string]string{
	"flash":      defaultTTSModel,
	"flash-lite": "gemini-3.8-flash-lite-tts",
}

// maxTTSResponseBytes bounds a unary response. The 16,384-token output limit
// is roughly 11 minutes of 24 kHz PCM, about 42 MB once base64 encoded.
const maxTTSResponseBytes = 128 << 20

// ttsTurn is one span of transcript. Speaker is empty for single-speaker text.
type ttsTurn struct {
	Speaker, Style, Text string
}

type ttsSpeaker struct {
	Name, Voice string
}

type speechRequest struct {
	Model    string
	Turns    []ttsTurn
	Voice    string       // Single-speaker voice: prebuilt name, library ID, voice_ or voicekey_.
	Speakers []ttsSpeaker // Native two-speaker dialogue; Voice must be empty.
	Language string
	Encoding string
	Seed     *int64
}

type ttsAnnotation struct {
	Type    string `json:"type"`
	Speaker string `json:"speaker,omitempty"`
	Style   string `json:"style,omitempty"`
}

type ttsContent struct {
	Type        string          `json:"type"`
	Text        string          `json:"text"`
	Annotations []ttsAnnotation `json:"annotations,omitempty"`
}

type ttsInput struct {
	Type    string       `json:"type"`
	Content []ttsContent `json:"content"`
}

type ttsResponseFormat struct {
	Type     string `json:"type"`
	MIMEType string `json:"mime_type,omitempty"`
}

type ttsVoiceConfig struct {
	Speaker  string `json:"speaker,omitempty"`
	Voice    string `json:"voice"`
	Language string `json:"language,omitempty"`
}

type ttsGenerationConfig struct {
	SpeechConfig any    `json:"speech_config"`
	Seed         *int64 `json:"seed,omitempty"`
}

type ttsInteractionRequest struct {
	Model            string              `json:"model"`
	Input            []ttsInput          `json:"input"`
	ResponseFormat   ttsResponseFormat   `json:"response_format"`
	GenerationConfig ttsGenerationConfig `json:"generation_config"`
	Stream           bool                `json:"stream"`
	Store            bool                `json:"store"`
}

type ttsOutputContent struct {
	Type       string `json:"type"`
	MIMEType   string `json:"mime_type"`
	Data       string `json:"data"`
	Text       string `json:"text"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type ttsUsage struct {
	TotalInputTokens  int `json:"total_input_tokens"`
	TotalOutputTokens int `json:"total_output_tokens"`
}

type ttsInteraction struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Steps []struct {
		Type    string             `json:"type"`
		Content []ttsOutputContent `json:"content"`
	} `json:"steps"`
	Usage ttsUsage `json:"usage"`
}

type ttsStreamEvent struct {
	EventType   string           `json:"event_type"`
	Delta       ttsOutputContent `json:"delta"`
	Interaction *ttsInteraction  `json:"interaction"`
	Status      string           `json:"status"`
	Error       struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// speechResult describes a finished synthesis. Raw holds the unary response.
type speechResult struct {
	Usage ttsUsage
	Raw   []byte
}

func (r speechRequest) payload(stream bool) ([]byte, error) {
	if strings.TrimSpace(r.Model) == "" {
		return nil, errors.New("a TTS model is required")
	}
	if len(r.Turns) == 0 {
		return nil, errors.New("text to speak is required")
	}
	content := make([]ttsContent, 0, len(r.Turns))
	for _, turn := range r.Turns {
		if strings.TrimSpace(turn.Text) == "" {
			return nil, errors.New("speech turns cannot be empty")
		}
		part := ttsContent{Type: "text", Text: turn.Text}
		if turn.Speaker != "" || turn.Style != "" {
			part.Annotations = []ttsAnnotation{{Type: "speech_metadata", Speaker: turn.Speaker, Style: turn.Style}}
		}
		content = append(content, part)
	}
	var speech any
	if len(r.Speakers) > 0 {
		if r.Voice != "" {
			return nil, errors.New("use either one voice or two speakers")
		}
		if len(r.Speakers) != 2 {
			return nil, errors.New("native multi-speaker speech requires exactly two speakers")
		}
		voices := make([]ttsVoiceConfig, 0, len(r.Speakers))
		for _, speaker := range r.Speakers {
			voices = append(voices, ttsVoiceConfig{Speaker: speaker.Name, Voice: speaker.Voice, Language: r.Language})
		}
		speech = map[string]any{"speakers": voices}
	} else {
		if strings.TrimSpace(r.Voice) == "" {
			return nil, errors.New("a voice is required")
		}
		speech = []ttsVoiceConfig{{Voice: r.Voice, Language: r.Language}}
	}
	encoding := r.Encoding
	if encoding == "" {
		encoding = encodingL16
	}
	return json.Marshal(ttsInteractionRequest{
		Model:            r.Model,
		Input:            []ttsInput{{Type: "user_input", Content: content}},
		ResponseFormat:   ttsResponseFormat{Type: "audio", MIMEType: audioFormat{Encoding: encoding}.requestMIME()},
		GenerationConfig: ttsGenerationConfig{SpeechConfig: speech, Seed: r.Seed},
		Stream:           stream,
		Store:            false,
	})
}

// synthesizeSpeech sends one Interactions request and passes audio to emit in
// order. Streaming emits each chunk as it arrives; unary emits once.
func (c *geminiClient) synthesizeSpeech(ctx context.Context, request speechRequest, stream bool, emit func(audioFormat, []byte) error) (speechResult, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return speechResult{}, errors.New("Gemini API key is required")
	}
	payload, err := request.payload(stream)
	if err != nil {
		return speechResult{}, err
	}
	response, err := c.apiRequest(ctx, http.MethodPost, "/interactions", bytes.NewReader(payload), stream)
	if err != nil {
		return speechResult{}, err
	}
	defer response.Body.Close()
	if err := c.apiStatusError(response, "Gemini TTS API"); err != nil {
		return speechResult{}, err
	}
	if stream {
		return c.readSpeechStream(response.Body, emit)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxTTSResponseBytes+1))
	if err != nil {
		return speechResult{}, fmt.Errorf("read Gemini TTS response: %w", err)
	}
	if len(raw) > maxTTSResponseBytes {
		return speechResult{}, errors.New("Gemini TTS response is too large")
	}
	var interaction ttsInteraction
	if err := json.Unmarshal(raw, &interaction); err != nil {
		return speechResult{}, fmt.Errorf("decode Gemini TTS response: %w", err)
	}
	if err := c.interactionStatusError(interaction.Status, interaction.Error.Message); err != nil {
		return speechResult{}, err
	}
	var details []string
	emitted := false
	for _, step := range interaction.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, part := range step.Content {
			switch {
			case part.Type == "audio" && part.Data != "":
				format, data, err := decodeSpeechAudio(part)
				if err != nil {
					return speechResult{}, err
				}
				if err := emit(format, data); err != nil {
					return speechResult{}, err
				}
				emitted = true
			case part.Type == "text" && strings.TrimSpace(part.Text) != "":
				details = append(details, part.Text)
			}
		}
	}
	if !emitted {
		if len(details) > 0 {
			return speechResult{}, fmt.Errorf("Gemini response did not contain audio: %s", c.boundedDiagnostic(strings.Join(details, "; ")))
		}
		return speechResult{}, errors.New("Gemini response did not contain audio")
	}
	return speechResult{Usage: interaction.Usage, Raw: raw}, nil
}

func (c *geminiClient) readSpeechStream(body io.Reader, emit func(audioFormat, []byte) error) (speechResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	var data []string
	var result speechResult
	completed := false
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if payload == "[DONE]" {
			return true, nil
		}
		var event ttsStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return false, fmt.Errorf("decode Gemini TTS stream event: %w", err)
		}
		switch event.EventType {
		case "step.delta":
			if event.Delta.Type != "audio" || event.Delta.Data == "" {
				return false, nil
			}
			format, audio, err := decodeSpeechAudio(event.Delta)
			if err != nil {
				return false, err
			}
			return false, emit(format, audio)
		case "error":
			message := event.Error.Message
			if message == "" {
				message = event.Error.Code
			}
			return false, fmt.Errorf("Gemini TTS stream failed: %s", c.boundedDiagnostic(message))
		case "interaction.completed":
			if event.Interaction != nil {
				if err := c.interactionStatusError(event.Interaction.Status, event.Interaction.Error.Message); err != nil {
					return false, err
				}
				result.Usage = event.Interaction.Usage
			}
			completed = true
		case "interaction.status_update":
			if event.Status == "failed" || event.Status == "cancelled" || event.Status == "incomplete" {
				return false, c.interactionStatusError(event.Status, "")
			}
		}
		return false, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := dispatch()
			if err != nil {
				return result, err
			}
			if done {
				return result, nil
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read Gemini TTS stream: %w", err)
	}
	if done, err := dispatch(); err != nil || done {
		return result, err
	}
	if !completed {
		return result, errors.New("Gemini TTS stream ended before completion")
	}
	return result, nil
}

func decodeSpeechAudio(part ttsOutputContent) (audioFormat, []byte, error) {
	data, err := base64.StdEncoding.DecodeString(part.Data)
	if err != nil {
		return audioFormat{}, nil, fmt.Errorf("decode Gemini audio data: %w", err)
	}
	mimeType := part.MIMEType
	if mimeType == "" {
		mimeType = "audio/l16"
	}
	format, wav, err := parseAudioMIME(mimeType)
	if err != nil {
		return audioFormat{}, nil, err
	}
	if wav || bytes.HasPrefix(data, []byte("RIFF")) {
		return splitWAV(data)
	}
	if part.SampleRate > 0 {
		format.SampleRate = part.SampleRate
	}
	if part.Channels > 0 {
		format.Channels = part.Channels
	}
	return format, data, nil
}

func (c *geminiClient) interactionStatusError(status, message string) error {
	if status == "" || status == "completed" {
		return nil
	}
	if detail := c.boundedDiagnostic(message); detail != "" {
		return fmt.Errorf("Gemini interaction %s: %s", c.boundedDiagnostic(status), detail)
	}
	return fmt.Errorf("Gemini interaction %s", c.boundedDiagnostic(status))
}

// apiRequest sends an authenticated JSON request below the Gemini base URL.
func (c *geminiClient) apiRequest(ctx context.Context, method, path string, body io.Reader, stream bool) (*http.Response, error) {
	return c.apiRequestWithHeaders(ctx, method, path, body, stream, nil)
}

func (c *geminiClient) apiRequestWithHeaders(ctx context.Context, method, path string, body io.Reader, stream bool, headers map[string]string) (*http.Response, error) {
	baseURL := c.baseURL
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, fmt.Errorf("create Gemini request: %w", err)
	}
	request.Header.Set("x-goog-api-key", c.apiKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("send Gemini request: %w", ctx.Err())
		}
		// The error text can include the request URL; never echo request headers.
		return nil, fmt.Errorf("send Gemini request: %s", c.redact(err.Error()))
	}
	return response, nil
}

func (c *geminiClient) apiStatusError(response *http.Response, label string) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	var apiErr geminiAPIError
	message := ""
	if json.Unmarshal(data, &apiErr) == nil {
		message = apiErr.Error.Message
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("%s returned %s: %s", label, response.Status, c.boundedDiagnostic(message))
}
