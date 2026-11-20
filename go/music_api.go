package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

const defaultMusicModel = "lyria-3.5"

var musicModels = map[string]string{
	"song": defaultMusicModel,
	"clip": "lyria-3-clip-preview",
}

const maxMusicImages = 10

// maxMusicResponseBytes bounds a response. Five minutes of 48 kHz stereo WAV
// is about 77 MB once base64 encoded.
const maxMusicResponseBytes = 256 << 20

// instrumentalMarker is the text Lyria returns in place of lyrics for a track
// without vocals.
const instrumentalMarker = "<instrumental>"

type musicRequest struct {
	Model  string
	Prompt string
	Images []imageData
	WAV    bool // Request WAV; Lyria returns MP3 otherwise.
}

type musicTrack struct {
	Data []byte
	Ext  string // ".mp3" or ".wav", from what Gemini actually returned.
}

// musicResult holds every audio block in response order. Text carries the
// lyrics and structure Lyria wrote for the track.
type musicResult struct {
	Tracks []musicTrack
	Text   []string
	Usage  ttsUsage
	Raw    []byte
}

type musicInteractionRequest struct {
	Model          string             `json:"model"`
	Input          any                `json:"input"`
	ResponseFormat *ttsResponseFormat `json:"response_format,omitempty"`
	Store          bool               `json:"store"`
}

func (r musicRequest) payload() ([]byte, error) {
	if strings.TrimSpace(r.Model) == "" {
		return nil, errors.New("a music model is required")
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return nil, errors.New("a music prompt is required")
	}
	if len(r.Images) > maxMusicImages {
		return nil, fmt.Errorf("music generation accepts up to %d reference images", maxMusicImages)
	}
	request := musicInteractionRequest{Model: r.Model, Input: r.Prompt}
	if len(r.Images) > 0 {
		parts := []omniInput{{Type: "text", Text: r.Prompt}}
		for _, image := range r.Images {
			if len(image.Data) == 0 || !strings.HasPrefix(strings.ToLower(image.MIMEType), "image/") {
				return nil, errors.New("music reference inputs must be nonempty images")
			}
			parts = append(parts, omniInput{Type: "image", MIMEType: image.MIMEType, Data: base64.StdEncoding.EncodeToString(image.Data)})
		}
		request.Input = parts
	}
	if r.WAV {
		request.ResponseFormat = &ttsResponseFormat{Type: "audio", MIMEType: "audio/wav"}
	}
	return json.Marshal(request)
}

// generateMusic sends one Interactions request. Music generation is single
// turn, so nothing is stored server-side.
func (c *geminiClient) generateMusic(ctx context.Context, request musicRequest) (musicResult, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return musicResult{}, errors.New("Gemini API key is required")
	}
	payload, err := request.payload()
	if err != nil {
		return musicResult{}, err
	}
	response, err := c.apiRequestWithHeaders(ctx, http.MethodPost, "/interactions", bytes.NewReader(payload), false, map[string]string{"Api-Revision": "2026-05-20"})
	if err != nil {
		return musicResult{}, err
	}
	defer response.Body.Close()
	if err := c.apiStatusError(response, "Gemini music API"); err != nil {
		return musicResult{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxMusicResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return musicResult{}, fmt.Errorf("read Gemini music response: %w", ctx.Err())
		}
		return musicResult{}, fmt.Errorf("read Gemini music response: %w", err)
	}
	if len(raw) > maxMusicResponseBytes {
		return musicResult{}, errors.New("Gemini music response is too large")
	}
	var interaction ttsInteraction
	if err := json.Unmarshal(raw, &interaction); err != nil {
		return musicResult{}, fmt.Errorf("decode Gemini music response: %w", err)
	}
	if err := c.interactionStatusError(interaction.Status, interaction.Error.Message); err != nil {
		return musicResult{}, err
	}
	result := musicResult{Usage: interaction.Usage, Raw: raw}
	for _, step := range interaction.Steps {
		if step.Type != "model_output" {
			continue
		}
		for _, part := range step.Content {
			switch {
			case part.Type == "audio" && part.Data != "":
				data, err := base64.StdEncoding.DecodeString(part.Data)
				if err != nil {
					return musicResult{}, fmt.Errorf("decode Gemini audio data: %w", err)
				}
				ext, err := musicAudioExt(part.MIMEType, data)
				if err != nil {
					return musicResult{}, err
				}
				result.Tracks = append(result.Tracks, musicTrack{Data: data, Ext: ext})
			case part.Type == "text" && strings.TrimSpace(part.Text) != "":
				result.Text = append(result.Text, c.redact(part.Text))
			}
		}
	}
	if len(result.Tracks) == 0 {
		if len(result.Text) > 0 {
			return musicResult{}, fmt.Errorf("Gemini response did not contain audio: %s", c.boundedDiagnostic(strings.Join(result.Text, "; ")))
		}
		return musicResult{}, errors.New("Gemini response did not contain audio")
	}
	return result, nil
}

// musicAudioExt names the container Gemini returned, trusting the bytes over
// the declared MIME type.
func musicAudioExt(mimeType string, data []byte) (string, error) {
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return ".wav", nil
	case bytes.HasPrefix(data, []byte("ID3")), len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return ".mp3", nil
	}
	mediaType, _, _ := mime.ParseMediaType(mimeType)
	switch mediaType {
	case "audio/mpeg", "audio/mp3":
		return ".mp3", nil
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav", nil
	}
	return "", fmt.Errorf("Gemini returned unsupported audio type %q", boundedDiagnostic(mimeType))
}
