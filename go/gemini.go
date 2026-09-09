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
	"strconv"
	"strings"
	"unicode/utf8"
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
	PromptFeedback geminiPromptFeedback `json:"promptFeedback"`
	Candidates     []geminiCandidate    `json:"candidates"`
}

type geminiPromptFeedback struct {
	BlockReason        string               `json:"blockReason"`
	BlockReasonMessage string               `json:"blockReasonMessage"`
	SafetyRatings      []geminiSafetyRating `json:"safetyRatings"`
}

type geminiCandidate struct {
	Content struct {
		Parts []geminiResponsePart `json:"parts"`
	} `json:"content"`
	FinishReason  string               `json:"finishReason"`
	SafetyRatings []geminiSafetyRating `json:"safetyRatings"`
}

type geminiResponsePart struct {
	Thought    bool              `json:"thought"`
	Text       string            `json:"text"`
	InlineData *geminiInlineData `json:"inlineData"`
}

type geminiSafetyRating struct {
	Category    string `json:"category"`
	Probability string `json:"probability"`
	Blocked     bool   `json:"blocked"`
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
		message = c.redact(message)
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
	diagnostics := c.responseDiagnostics(decoded)
	if diagnostics != "" {
		return imageData{}, fmt.Errorf("Gemini response did not contain an image: %s", c.redact(diagnostics))
	}
	return imageData{}, fmt.Errorf("Gemini response did not contain an image")
}

const maxGeminiDiagnosticLength = 800

func (c *geminiClient) redact(value string) string {
	if c.apiKey == "" {
		return value
	}
	return strings.ReplaceAll(value, c.apiKey, "[REDACTED]")
}

func (c *geminiClient) responseDiagnostics(response geminiResponse) string {
	var diagnostics []string
	feedback := response.PromptFeedback
	if feedback.BlockReason != "" {
		diagnostics = append(diagnostics, "prompt blocked: "+feedback.BlockReason)
	}
	if feedback.BlockReasonMessage != "" {
		diagnostics = append(diagnostics, "prompt feedback: "+c.boundedDiagnostic(feedback.BlockReasonMessage))
	}
	if ratings := safetyRatingsDiagnostic(feedback.SafetyRatings); ratings != "" {
		diagnostics = append(diagnostics, "prompt safety ratings: "+ratings)
	}
	for _, candidate := range response.Candidates {
		if candidate.FinishReason != "" {
			diagnostics = append(diagnostics, "candidate finish reason: "+candidate.FinishReason)
		}
		if ratings := safetyRatingsDiagnostic(candidate.SafetyRatings); ratings != "" {
			diagnostics = append(diagnostics, "candidate safety ratings: "+ratings)
		}
		for _, part := range candidate.Content.Parts {
			if !part.Thought && strings.TrimSpace(part.Text) != "" {
				diagnostics = append(diagnostics, "model text: "+strconv.Quote(c.boundedDiagnostic(part.Text)))
			}
		}
	}
	return c.boundedDiagnostic(strings.Join(diagnostics, "; "))
}

func safetyRatingsDiagnostic(ratings []geminiSafetyRating) string {
	parts := make([]string, 0, len(ratings))
	for _, rating := range ratings {
		if rating.Category == "" && rating.Probability == "" && !rating.Blocked {
			continue
		}
		entry := rating.Category
		if rating.Probability != "" {
			entry += " (" + rating.Probability + ")"
		}
		if rating.Blocked {
			entry += " blocked"
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, ", ")
}

func boundedDiagnostic(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maxGeminiDiagnosticLength {
		return value
	}
	end := maxGeminiDiagnosticLength
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}

func (c *geminiClient) boundedDiagnostic(value string) string {
	return boundedDiagnostic(c.redact(value))
}
