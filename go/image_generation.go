package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type imageRequestOptions struct {
	Aspect, Size, Thinking, Search string
	IncludeThoughts, Text          bool
}

type imageSegment struct {
	Kind       string `json:"kind"`
	Text       string `json:"text,omitempty"`
	ImageIndex int    `json:"imageIndex,omitempty"`
	Thought    bool   `json:"thought,omitempty"`
}

type imageGenerationResult struct {
	RawResponse      []byte
	ModelContent     json.RawMessage
	Images           []imageData
	ThoughtImages    []imageData
	Segments         []imageSegment
	Grounding        json.RawMessage
	SearchEntryPoint string
	Diagnostics      string
}

type imageGenerationRequest struct {
	Model    string
	Contents []json.RawMessage
	Options  imageRequestOptions
}

func buildImageRequest(model string, contents []json.RawMessage, opts imageRequestOptions) ([]byte, error) {
	if strings.TrimSpace(model) == "" || len(contents) == 0 {
		return nil, errors.New("model and content are required")
	}
	modalities := []string{"IMAGE"}
	if opts.Text {
		modalities = []string{"TEXT", "IMAGE"}
	}
	config := struct {
		ResponseModalities []string `json:"responseModalities"`
		ImageConfig        *struct {
			AspectRatio string `json:"aspectRatio,omitempty"`
			ImageSize   string `json:"imageSize,omitempty"`
		} `json:"imageConfig,omitempty"`
		ThinkingConfig *struct {
			ThinkingLevel   string `json:"thinkingLevel,omitempty"`
			IncludeThoughts bool   `json:"includeThoughts,omitempty"`
		} `json:"thinkingConfig,omitempty"`
	}{ResponseModalities: modalities}
	if opts.Aspect != "" || opts.Size != "" {
		config.ImageConfig = &struct {
			AspectRatio string `json:"aspectRatio,omitempty"`
			ImageSize   string `json:"imageSize,omitempty"`
		}{AspectRatio: opts.Aspect, ImageSize: opts.Size}
	}
	if opts.Thinking != "" || opts.IncludeThoughts {
		config.ThinkingConfig = &struct {
			ThinkingLevel   string `json:"thinkingLevel,omitempty"`
			IncludeThoughts bool   `json:"includeThoughts,omitempty"`
		}{ThinkingLevel: strings.ToUpper(opts.Thinking), IncludeThoughts: opts.IncludeThoughts}
	}
	var tools []any
	if opts.Search != "" && opts.Search != "none" {
		searchTypes := map[string]any{}
		if opts.Search == "web" || opts.Search == "both" {
			searchTypes["webSearch"] = map[string]any{}
		}
		if opts.Search == "image" || opts.Search == "both" {
			searchTypes["imageSearch"] = map[string]any{}
		}
		tools = []any{map[string]any{"googleSearch": map[string]any{"searchTypes": searchTypes}}}
	}
	return json.Marshal(struct {
		Contents         []json.RawMessage `json:"contents"`
		GenerationConfig any               `json:"generationConfig"`
		Tools            []any             `json:"tools,omitempty"`
	}{Contents: contents, GenerationConfig: config, Tools: tools})
}

func (c *geminiClient) generateImageAdvanced(ctx context.Context, request imageGenerationRequest) (imageGenerationResult, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return imageGenerationResult{}, errors.New("Gemini API key is required")
	}
	payload, err := buildImageRequest(request.Model, request.Contents, request.Options)
	if err != nil {
		return imageGenerationResult{}, err
	}
	baseURL := c.baseURL
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/models/" + url.PathEscape(request.Model) + ":generateContent"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return imageGenerationResult{}, fmt.Errorf("create Gemini image request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-goog-api-key", c.apiKey)
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return imageGenerationResult{}, ctx.Err()
		}
		return imageGenerationResult{}, errors.New("Gemini image request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return imageGenerationResult{}, fmt.Errorf("read Gemini image response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiErr geminiAPIError
		message := http.StatusText(response.StatusCode)
		if json.Unmarshal(body, &apiErr) == nil && strings.TrimSpace(apiErr.Error.Message) != "" {
			message = apiErr.Error.Message
		}
		return imageGenerationResult{}, fmt.Errorf("Gemini API returned %s: %s", response.Status, c.boundedDiagnostic(message))
	}
	result, err := decodeImageResponse(body)
	if err != nil {
		return result, err
	}
	if len(result.Images) == 0 && !(request.Options.Text && result.hasVisibleText()) {
		if result.Diagnostics != "" {
			return result, fmt.Errorf("Gemini response did not contain an image: %s", c.boundedDiagnostic(result.Diagnostics))
		}
		return result, errors.New("Gemini response did not contain an image")
	}
	return result, nil
}

func decodeImageResponse(body []byte) (imageGenerationResult, error) {
	result := imageGenerationResult{RawResponse: append([]byte(nil), body...)}
	var response struct {
		PromptFeedback geminiPromptFeedback `json:"promptFeedback"`
		Candidates     []struct {
			Content           json.RawMessage `json:"content"`
			FinishReason      string          `json:"finishReason"`
			GroundingMetadata json.RawMessage `json:"groundingMetadata"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return result, fmt.Errorf("decode Gemini image response: %w", err)
	}
	var diagnostics []string
	if response.PromptFeedback.BlockReason != "" {
		diagnostics = append(diagnostics, "prompt blocked: "+response.PromptFeedback.BlockReason)
	}
	if response.PromptFeedback.BlockReasonMessage != "" {
		diagnostics = append(diagnostics, "prompt feedback: "+response.PromptFeedback.BlockReasonMessage)
	}
	for candidateIndex, candidate := range response.Candidates {
		if candidate.FinishReason != "" && candidate.FinishReason != "STOP" {
			diagnostics = append(diagnostics, "candidate finish reason: "+candidate.FinishReason)
		}
		if len(candidate.Content) == 0 || string(candidate.Content) == "null" {
			continue
		}
		var content struct {
			Parts []struct {
				Text       string            `json:"text"`
				Thought    bool              `json:"thought"`
				InlineData *geminiInlineData `json:"inlineData"`
			} `json:"parts"`
		}
		if err := json.Unmarshal(candidate.Content, &content); err != nil {
			return result, fmt.Errorf("decode Gemini image content: %w", err)
		}
		if len(result.ModelContent) == 0 {
			result.ModelContent = append(json.RawMessage(nil), candidate.Content...)
			result.Grounding = append(json.RawMessage(nil), candidate.GroundingMetadata...)
			result.SearchEntryPoint = searchEntryPoint(result.Grounding)
		}
		for _, part := range content.Parts {
			if part.Text != "" {
				result.Segments = append(result.Segments, imageSegment{Kind: "text", Text: part.Text, Thought: part.Thought})
			}
			if part.InlineData == nil || part.InlineData.Data == "" || !strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "image/") {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return result, fmt.Errorf("decode Gemini image data: %w", err)
			}
			if len(data) == 0 {
				continue
			}
			if part.Thought {
				result.ThoughtImages = append(result.ThoughtImages, imageData{Data: data, MIMEType: part.InlineData.MIMEType})
				result.Segments = append(result.Segments, imageSegment{Kind: "image", ImageIndex: len(result.ThoughtImages), Thought: true})
				continue
			}
			result.Images = append(result.Images, imageData{Data: data, MIMEType: part.InlineData.MIMEType})
			result.Segments = append(result.Segments, imageSegment{Kind: "image", ImageIndex: len(result.Images)})
		}
		if len(result.Images) > 0 || result.hasVisibleText() {
			break
		}
		if candidateIndex < len(response.Candidates)-1 {
			result.ModelContent = nil
			result.Grounding = nil
			result.SearchEntryPoint = ""
			result.ThoughtImages = nil
			result.Segments = nil
		}
	}
	result.Diagnostics = boundedDiagnostic(strings.Join(diagnostics, "; "))
	return result, nil
}

func (r imageGenerationResult) hasVisibleText() bool {
	for _, segment := range r.Segments {
		if segment.Kind == "text" && !segment.Thought && strings.TrimSpace(segment.Text) != "" {
			return true
		}
	}
	return false
}

func searchEntryPoint(grounding json.RawMessage) string {
	var value struct {
		SearchEntryPoint struct {
			RenderedContent string `json:"renderedContent"`
		} `json:"searchEntryPoint"`
	}
	if json.Unmarshal(grounding, &value) != nil {
		return ""
	}
	return value.SearchEntryPoint.RenderedContent
}

func imageContent(prompt string, parts []geminiPart) (json.RawMessage, error) {
	all := make([]geminiPart, 0, len(parts)+1)
	all = append(all, geminiPart{Text: prompt})
	all = append(all, parts...)
	content, err := json.Marshal(geminiContent{Role: "user", Parts: all})
	if err != nil {
		return nil, fmt.Errorf("encode image prompt: %w", err)
	}
	return content, nil
}

func imageInline(data imageData) geminiPart {
	return geminiPart{InlineData: &geminiInlineData{MIMEType: data.MIMEType, Data: base64.StdEncoding.EncodeToString(data.Data)}}
}

func numberedImagePath(output string, number int) string {
	if number <= 1 {
		return output
	}
	ext := filepath.Ext(output)
	return strings.TrimSuffix(output, ext) + "-" + strconv.Itoa(number) + ext
}

func saveImageResult(output string, result imageGenerationResult, responseJSON string, includeThoughts bool) error {
	for i, image := range result.Images {
		if err := saveImageAtomic(numberedImagePath(output, i+1), image); err != nil {
			return err
		}
	}
	stem := strings.TrimSuffix(output, filepath.Ext(output))
	if len(result.Images) > 1 || result.hasVisibleText() {
		var manifest strings.Builder
		manifest.WriteString("# Generated result\n\n")
		for _, segment := range result.Segments {
			if segment.Thought {
				continue
			}
			switch segment.Kind {
			case "text":
				manifest.WriteString(strings.TrimSpace(segment.Text) + "\n\n")
			case "image":
				if segment.ImageIndex > 0 && segment.ImageIndex <= len(result.Images) {
					path := filepath.Base(numberedImagePath(output, segment.ImageIndex))
					manifest.WriteString("![Generated image " + strconv.Itoa(segment.ImageIndex) + "](" + url.PathEscape(path) + ")\n\n")
				}
			}
		}
		if err := atomicWriteFile(stem+".md", []byte(manifest.String()), 0o644); err != nil {
			return err
		}
	}
	if includeThoughts {
		var thoughts strings.Builder
		for _, segment := range result.Segments {
			if !segment.Thought {
				continue
			}
			switch segment.Kind {
			case "text":
				thoughts.WriteString(segment.Text + "\n\n")
			case "image":
				if segment.ImageIndex > 0 && segment.ImageIndex <= len(result.ThoughtImages) {
					path := stem + "-thought-" + strconv.Itoa(segment.ImageIndex) + ".png"
					if err := saveImageAtomic(path, result.ThoughtImages[segment.ImageIndex-1]); err != nil {
						return err
					}
					thoughts.WriteString("![Thought image " + strconv.Itoa(segment.ImageIndex) + "](" + url.PathEscape(filepath.Base(path)) + ")\n\n")
				}
			}
		}
		if thoughts.Len() > 0 {
			if err := atomicWriteFile(stem+"-thoughts.md", []byte(thoughts.String()), 0o600); err != nil {
				return err
			}
		}
	}
	if len(result.Grounding) > 0 && string(result.Grounding) != "null" {
		if err := saveGrounding(stem+"-grounding.html", result); err != nil {
			return err
		}
	}
	if responseJSON != "" {
		if err := atomicWriteFile(responseJSON, result.RawResponse, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func saveImageAtomic(path string, image imageData) error {
	ext := filepath.Ext(path)
	file, err := os.CreateTemp(filepath.Dir(path), ".generate-*"+ext)
	if err != nil {
		return fmt.Errorf("create image output: %w", err)
	}
	tempPath := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}
	defer os.Remove(tempPath)
	if err := saveImage(tempPath, image); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("save image %q: %w", path, err)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".generate-*")
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %q: %w", path, err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save %q: %w", path, err)
	}
	return nil
}

type groundingSource struct {
	URL, Title string
}

func groundingSources(metadata json.RawMessage) []groundingSource {
	var value struct {
		GroundingChunks []struct {
			Web struct {
				URI, Title string
			} `json:"web"`
			Image struct {
				URI       string `json:"uri"`
				SourceURL string `json:"sourceUrl"`
				PageURL   string `json:"pageUrl"`
				Title     string `json:"title"`
				Web       struct {
					URI string `json:"uri"`
				} `json:"web"`
			} `json:"image"`
		} `json:"groundingChunks"`
	}
	if json.Unmarshal(metadata, &value) != nil {
		return nil
	}
	var sources []groundingSource
	seen := make(map[string]bool)
	add := func(rawURL, title string) {
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || seen[rawURL] {
			return
		}
		seen[rawURL] = true
		sources = append(sources, groundingSource{URL: rawURL, Title: title})
	}
	for _, chunk := range value.GroundingChunks {
		add(chunk.Web.URI, chunk.Web.Title)
		if chunk.Image.URI != "" {
			add(chunk.Image.URI, chunk.Image.Title)
		} else if chunk.Image.PageURL != "" {
			add(chunk.Image.PageURL, chunk.Image.Title)
		} else if chunk.Image.SourceURL != "" {
			add(chunk.Image.SourceURL, chunk.Image.Title)
		} else {
			add(chunk.Image.Web.URI, chunk.Image.Title)
		}
	}
	return sources
}

func saveGrounding(path string, result imageGenerationResult) error {
	var htmlPage strings.Builder
	htmlPage.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Image sources</title></head><body><h1>Image sources</h1>")
	if result.SearchEntryPoint != "" {
		htmlPage.WriteString("<iframe sandbox=\"allow-popups allow-popups-to-escape-sandbox\" title=\"Search attribution\" srcdoc=\"")
		htmlPage.WriteString(html.EscapeString(result.SearchEntryPoint))
		htmlPage.WriteString("\"></iframe>")
	}
	htmlPage.WriteString("<ul>")
	for _, source := range groundingSources(result.Grounding) {
		label := source.Title
		if label == "" {
			label = source.URL
		}
		htmlPage.WriteString("<li><a href=\"" + html.EscapeString(source.URL) + "\">" + html.EscapeString(label) + "</a></li>")
	}
	htmlPage.WriteString("</ul></body></html>")
	return atomicWriteFile(path, []byte(htmlPage.String()), 0o644)
}
