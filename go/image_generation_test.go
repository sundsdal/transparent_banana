package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdvancedImageRequestSettingsAndRawContent(t *testing.T) {
	modelPart := json.RawMessage(`{"role":"model","parts":[{"text":"prior","thoughtSignature":"opaque-signature","future":{"key":"kept"}}]}`)
	userPart := json.RawMessage(`{"role":"user","parts":[{"text":"next"}]}`)
	payload, err := buildImageRequest(defaultModel, []json.RawMessage{modelPart, userPart}, imageRequestOptions{
		Aspect: "16:9", Size: "4K", Thinking: "high", IncludeThoughts: true, Search: "both", Text: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	contents := request["contents"].([]any)
	first := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if first["thoughtSignature"] != "opaque-signature" || first["future"].(map[string]any)["key"] != "kept" {
		t.Fatalf("raw model content changed: %#v", first)
	}
	config := request["generationConfig"].(map[string]any)
	imageConfig := config["imageConfig"].(map[string]any)
	thinking := config["thinkingConfig"].(map[string]any)
	if imageConfig["aspectRatio"] != "16:9" || imageConfig["imageSize"] != "4K" || thinking["thinkingLevel"] != "HIGH" || thinking["includeThoughts"] != true {
		t.Fatalf("generation config: %#v", config)
	}
	modalities := config["responseModalities"].([]any)
	if len(modalities) != 2 || modalities[0] != "TEXT" || modalities[1] != "IMAGE" {
		t.Fatalf("modalities: %#v", modalities)
	}
	searchTypes := request["tools"].([]any)[0].(map[string]any)["googleSearch"].(map[string]any)["searchTypes"].(map[string]any)
	if _, ok := searchTypes["webSearch"]; !ok {
		t.Fatal("missing web search")
	}
	if _, ok := searchTypes["imageSearch"]; !ok {
		t.Fatal("missing image search")
	}
}

func TestAdvancedImageHTTPTextOnlyAndSafety(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/models/"+defaultModel+":generateContent" || r.Header.Get("x-goog-api-key") != "private-key" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"answer","thoughtSignature":"secret-thought"}]}}]}`)
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "private-key", baseURL: server.URL, httpClient: server.Client()}
	content := json.RawMessage(`{"role":"user","parts":[{"text":"question"}]}`)
	result, err := client.generateImageAdvanced(context.Background(), imageGenerationRequest{Model: defaultModel, Contents: []json.RawMessage{content}, Options: imageRequestOptions{Text: true}})
	if err != nil || !result.hasVisibleText() || !bytes.Contains(result.ModelContent, []byte(`"thoughtSignature":"secret-thought"`)) {
		t.Fatalf("text-only result = %+v, %v", result, err)
	}
	_, err = client.generateImageAdvanced(context.Background(), imageGenerationRequest{Model: defaultModel, Contents: []json.RawMessage{content}})
	if err == nil || !strings.Contains(err.Error(), "did not contain an image") {
		t.Fatalf("image-only text result error = %v", err)
	}
}

func TestDecodeImageResponseAndSaveAllOutputs(t *testing.T) {
	one := fixtureImage(t, color.NRGBA{R: 40, A: 255})
	two := fixtureImage(t, color.NRGBA{B: 50, A: 255})
	thought := fixtureImage(t, color.NRGBA{G: 60, A: 255})
	response := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"role": "model", "parts": []any{
			map[string]any{"thought": true, "text": "private reasoning"},
			map[string]any{"thought": true, "inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(thought.Data)}},
			map[string]any{"text": "First image"},
			map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(one.Data)}},
			map[string]any{"text": "Second image"},
			map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(two.Data)}},
		}},
		"groundingMetadata": map[string]any{
			"searchEntryPoint": map[string]any{"renderedContent": `<a href="https://example.com">Search</a>`},
			"groundingChunks":  []any{map[string]any{"image": map[string]any{"uri": "https://example.com/containing-page", "image_uri": "https://example.com/raw.png", "title": "Source page"}}},
		},
	}}}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeImageResponse(body)
	if err != nil || len(result.Images) != 2 || len(result.ThoughtImages) != 1 || result.SearchEntryPoint == "" {
		t.Fatalf("decoded = %+v, %v", result, err)
	}
	dir := t.TempDir()
	output := filepath.Join(dir, "result.png")
	if err := saveImageResult(output, result, filepath.Join(dir, "raw.json"), true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"result.png", "result-2.png", "result.md", "result-thoughts.md", "result-thought-1.png", "result-grounding.html", "raw.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	manifest, _ := os.ReadFile(filepath.Join(dir, "result.md"))
	if !strings.Contains(string(manifest), "First image") || !strings.Contains(string(manifest), "result-2.png") || strings.Contains(string(manifest), "private reasoning") {
		t.Fatalf("manifest = %q", manifest)
	}
	html, _ := os.ReadFile(filepath.Join(dir, "result-grounding.html"))
	if !strings.Contains(string(html), "https://example.com/containing-page") || strings.Contains(string(html), "https://example.com/raw.png") {
		t.Fatalf("grounding page = %q", html)
	}
}

type fakeAdvancedImageGenerator struct {
	requests []imageGenerationRequest
	results  []imageGenerationResult
	err      error
}

func (f *fakeAdvancedImageGenerator) prepareImageInputs(_ context.Context, paths []string) ([]geminiPart, error) {
	if len(paths) > 0 {
		return []geminiPart{{Text: "input marker"}}, nil
	}
	return nil, nil
}

func (f *fakeAdvancedImageGenerator) generateImageAdvanced(_ context.Context, request imageGenerationRequest) (imageGenerationResult, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return imageGenerationResult{}, f.err
	}
	if len(f.results) == 0 {
		return imageGenerationResult{}, errors.New("missing fake result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result, nil
}

func TestImageSessionReplayAndFailurePreservation(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	firstContent := json.RawMessage(`{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="},"thoughtSignature":"opaque-sig","future":{"saved":true}}]}`)
	image := fixtureImage(t, color.NRGBA{R: 100, A: 255})
	first := imageGenerationResult{ModelContent: firstContent, Images: []imageData{image}, Segments: []imageSegment{{Kind: "image", ImageIndex: 1}}, RawResponse: []byte(`{"candidates":[]}`)}
	f := &fakeAdvancedImageGenerator{results: []imageGenerationResult{first}}
	o := options{prompt: "first turn", model: defaultModel, output: filepath.Join(dir, "first.png"), session: sessionPath, imageSize: "2K", thinking: "high"}
	if err := runAdvancedImage(context.Background(), o, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	saved, err := loadImageSession(sessionPath, defaultModel)
	if err != nil || len(saved.Contents) != 2 || !bytes.Contains(compactJSON(t, saved.Contents[1]), []byte(`"thoughtSignature":"opaque-sig"`)) {
		t.Fatalf("session = %+v, %v", saved, err)
	}
	o.prompt, o.output = "second turn", filepath.Join(dir, "second.png")
	f.results = []imageGenerationResult{first}
	if err := runAdvancedImage(context.Background(), o, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 2 || len(f.requests[1].Contents) != 3 || !bytes.Contains(compactJSON(t, f.requests[1].Contents[1]), []byte(`"future":{"saved":true}`)) || f.requests[1].Options.Size != "2K" {
		t.Fatalf("replay request = %+v", f.requests)
	}
	before, _ := os.ReadFile(sessionPath)
	o.prompt, o.output = "failed output", filepath.Join(dir, "missing-directory", "third.png")
	f.results = []imageGenerationResult{first}
	if err := runAdvancedImage(context.Background(), o, f, io.Discard); err == nil {
		t.Fatal("output failure unexpectedly succeeded")
	}
	after, _ := os.ReadFile(sessionPath)
	if !bytes.Equal(before, after) {
		t.Fatal("session advanced after output failure")
	}
}

func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}
