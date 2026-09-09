package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

type imageData struct {
	Data     []byte
	MIMEType string
}

type geminiClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	ImageConfig        *geminiImageConfig `json:"imageConfig,omitempty"`
}

type geminiImageConfig struct {
	AspectRatio string `json:"aspectRatio"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Thought    bool              `json:"thought"`
				InlineData *geminiInlineData `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

type geminiAPIError struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// generate creates one image with Gemini's generateContent endpoint. The caller
// owns timeouts and cancellation through ctx and c.httpClient.
func (c *geminiClient) generate(ctx context.Context, model, prompt string, images []imageData, aspect string) (imageData, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return imageData{}, fmt.Errorf("Gemini API key is required")
	}
	if strings.TrimSpace(model) == "" {
		return imageData{}, fmt.Errorf("Gemini model is required")
	}

	parts := make([]geminiPart, 0, len(images)+1)
	if prompt != "" {
		parts = append(parts, geminiPart{Text: prompt})
	}
	for _, image := range images {
		if len(image.Data) == 0 {
			return imageData{}, fmt.Errorf("input image is empty")
		}
		if strings.TrimSpace(image.MIMEType) == "" {
			return imageData{}, fmt.Errorf("input image MIME type is required")
		}
		parts = append(parts, geminiPart{InlineData: &geminiInlineData{
			MIMEType: image.MIMEType,
			Data:     base64.StdEncoding.EncodeToString(image.Data),
		}})
	}
	if len(parts) == 0 {
		return imageData{}, fmt.Errorf("a prompt or input image is required")
	}

	config := geminiGenerationConfig{ResponseModalities: []string{"IMAGE"}}
	if aspect != "" {
		config.ImageConfig = &geminiImageConfig{AspectRatio: aspect}
	}
	payload, err := json.Marshal(geminiRequest{
		Contents:         []geminiContent{{Role: "user", Parts: parts}},
		GenerationConfig: config,
	})
	if err != nil {
		return imageData{}, fmt.Errorf("encode Gemini request: %w", err)
	}

	baseURL := c.baseURL
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/models/" + url.PathEscape(model) + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return imageData{}, fmt.Errorf("create Gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return imageData{}, fmt.Errorf("send Gemini request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return imageData{}, fmt.Errorf("read Gemini response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var apiErr geminiAPIError
		message := ""
		if json.Unmarshal(body, &apiErr) == nil {
			message = strings.TrimSpace(apiErr.Error.Message)
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		message = strings.ReplaceAll(message, c.apiKey, "[REDACTED]")
		return imageData{}, fmt.Errorf("Gemini API returned %s: %s", resp.Status, message)
	}

	var decoded geminiResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return imageData{}, fmt.Errorf("decode Gemini response: %w", err)
	}
	for _, candidate := range decoded.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Thought || part.InlineData == nil || part.InlineData.Data == "" || !strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/") {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return imageData{}, fmt.Errorf("decode Gemini image data: %w", err)
			}
			if len(data) == 0 {
				continue
			}
			return imageData{Data: data, MIMEType: part.InlineData.MIMEType}, nil
		}
	}
	return imageData{}, fmt.Errorf("Gemini response did not contain an image")
}
