package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchStatusCancelAndDownload(t *testing.T) {
	var pngBytes bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 250, A: 255})
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{"key": "../unsafe", "response": map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "hello"}, map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(pngBytes.Bytes())}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	raw := append(append(line, '\n'), []byte(`{"key":"bad","error":{"message":"no image"}}`)...)
	raw = append(raw, '\n')
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("missing API key")
		}
		switch r.URL.Path {
		case "/v1beta/batches/abc":
			_, _ = w.Write([]byte(`{"name":"batches/abc","done":true,"metadata":{"state":"JOB_STATE_SUCCEEDED"},"response":{"responsesFile":"files/result-1"}}`))
		case "/v1beta/batches/abc:cancel":
			if r.Method != http.MethodPost {
				t.Errorf("cancel method %s", r.Method)
			}
			_, _ = w.Write([]byte(`{}`))
		case "/download/v1beta/files/result-1:download":
			if r.URL.Query().Get("alt") != "media" {
				t.Error("missing alt=media")
			}
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	var output bytes.Buffer
	if err := client.runBatch(context.Background(), []string{"status", "batches/abc"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"JOB_STATE_SUCCEEDED"`) {
		t.Fatal(output.String())
	}
	output.Reset()
	if err := client.runBatch(context.Background(), []string{"cancel", "batches/abc"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Cancellation requested") {
		t.Fatal(output.String())
	}
	output.Reset()
	dir := t.TempDir()
	if err := client.runBatch(context.Background(), []string{"download", "batches/abc", "--output-dir", dir}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "1 images, 1 text files, 1 request errors") {
		t.Fatal(output.String())
	}
	stored, err := os.ReadFile(filepath.Join(dir, "batch-results.jsonl"))
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("raw results not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0001-___unsafe.png")); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(dir, "0001-___unsafe.md"))
	if err != nil || !strings.Contains(string(text), "hello") {
		t.Fatalf("text extraction: %q, %v", text, err)
	}
	if len(paths) != 4 {
		t.Fatalf("requests: %v", paths)
	}
}

func TestBatchManifestValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.jsonl")
	if err := os.WriteFile(path, []byte(`{"key":"one","request":{"contents":[{"parts":[{"text":"cat"}]}],"generationConfig":{"responseModalities":["IMAGE"]}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBatchManifest(path, defaultModel); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		`{"key":"one","request":{"contents":[{}]}}` + "\n" + `{"key":"one","request":{"contents":[{}]}}`,
		`{"key":"one","request":{"model":"other","contents":[{}]}}`,
		`{"key":"one","request":{}}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateBatchManifest(path, defaultModel); err == nil {
			t.Fatalf("accepted invalid manifest: %s", data)
		}
	}
}

func TestBatchSubmitUploadsManifestAndCreatesJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	manifest := `{"key":"one","request":{"contents":[{"parts":[{"text":"a cat"}]}]}}` + "\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var uploadURL string
	var seenUpload, seenCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			if r.Header.Get("X-Goog-Upload-Header-Content-Type") != "application/jsonl" {
				t.Errorf("upload MIME: %q", r.Header.Get("X-Goog-Upload-Header-Content-Type"))
			}
			w.Header().Set("X-Goog-Upload-URL", uploadURL)
		case "/upload/v1beta/transfer":
			data := new(bytes.Buffer)
			_, _ = data.ReadFrom(r.Body)
			if data.String() != manifest {
				t.Errorf("uploaded manifest: %q", data.String())
			}
			seenUpload = true
			_, _ = w.Write([]byte(`{"file":{"name":"files/input-1","uri":"https://example.test/input-1","state":"ACTIVE"}}`))
		case "/v1beta/models/gemini-nano-banana-2.1:batchGenerateContent":
			if !seenUpload {
				t.Error("created batch before upload")
			}
			var body struct {
				Batch struct {
					DisplayName string `json:"displayName"`
					InputConfig struct {
						FileName string `json:"fileName"`
					} `json:"inputConfig"`
				} `json:"batch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Batch.DisplayName != "my batch" || body.Batch.InputConfig.FileName != "files/input-1" {
				t.Errorf("batch payload: %+v", body)
			}
			seenCreate = true
			_, _ = w.Write([]byte(`{"name":"batches/job-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	uploadURL = server.URL + "/upload/v1beta/transfer"
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	var out bytes.Buffer
	if err := client.runBatch(context.Background(), []string{"submit", path, "--name", "my batch", "--model", "flash"}, &out); err != nil {
		t.Fatal(err)
	}
	if !seenCreate || out.String() != "Submitted: batches/job-1\n" {
		t.Fatalf("submit result: %q", out.String())
	}
}

func TestBatchRejectsInvalidNamesBeforeNetwork(t *testing.T) {
	client := &geminiClient{apiKey: "test-key"}
	for _, name := range []string{"../secret", "batches/../secret", "batches/abc?x=1", "batches/"} {
		if err := client.runBatch(context.Background(), []string{"status", name}, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestBatchExtractDirectResponseAndInlineResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "direct.jsonl")
	if err := os.WriteFile(path, []byte(`{"candidates":[{"content":{"parts":[{"text":"direct answer"}]}}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	images, texts, failures, err := extractBatchResults(path, dir)
	if err != nil || images != 0 || texts != 1 || failures != 0 {
		t.Fatalf("direct result: %d %d %d %v", images, texts, failures, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0001-request.md")); err != nil {
		t.Fatal(err)
	}
	inlinePath := filepath.Join(dir, "inline.jsonl")
	inline := json.RawMessage(`{"inlinedResponses":[{"metadata":{"key":"first"},"response":{"candidates":[{"content":{"parts":[{"text":"inline answer"}]}}]}},{"metadata":{"key":"second"},"error":{"message":"blocked"}}]}`)
	if err := writeInlineBatchResults(inlinePath, inline); err != nil {
		t.Fatal(err)
	}
	images, texts, failures, err = extractBatchResults(inlinePath, dir)
	if err != nil || images != 0 || texts != 1 || failures != 1 {
		t.Fatalf("inline result: %d %d %d %v", images, texts, failures, err)
	}
	info, err := os.Stat(inlinePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("inline raw file permissions: %v %v", info, err)
	}
}

func TestBatchExtractLargeRecordAndFinalLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	write := func(data []byte) {
		t.Helper()
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write([]byte(`{"key":"large","padding":"`))
	padding := bytes.Repeat([]byte("x"), 1024*1024)
	for range 64 {
		write(padding)
	}
	image := fixtureImage(t, color.NRGBA{R: 200, A: 255})
	write([]byte(`","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString(image.Data) + `"}}]}}]}}` + "\r\n"))
	write([]byte("  \r\n"))
	write([]byte(`{"key":"last","response":{"candidates":[{"content":{"parts":[{"text":"final answer"}]}}]}}`))
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	images, texts, failures, err := extractBatchResults(path, dir)
	if err != nil || images != 1 || texts != 1 || failures != 0 {
		t.Fatalf("large result: %d images, %d texts, %d failures, %v", images, texts, failures, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0001-large.png")); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(dir, "0003-last.md"))
	if err != nil || !strings.Contains(string(text), "final answer") {
		t.Fatalf("final unterminated result: %q, %v", text, err)
	}
}

func TestBatchExtractMalformedFinalLineReportsPhysicalLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "malformed.jsonl")
	if err := os.WriteFile(path, []byte("{}\r\n\n{\"key\":\"broken\","), 0o600); err != nil {
		t.Fatal(err)
	}
	images, texts, failures, err := extractBatchResults(path, dir)
	if images != 0 || texts != 0 || failures != 1 || err == nil || !strings.Contains(err.Error(), "batch result line 3") {
		t.Fatalf("malformed final result: %d images, %d texts, %d failures, %v", images, texts, failures, err)
	}
}
