package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeMediaTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrepareImageInputsInlineFormats(t *testing.T) {
	files := []struct {
		name string
		data []byte
		mime string
	}{
		{"a.png", append([]byte("\x89PNG\r\n\x1a\n"), 0), "image/png"},
		{"b.jpg", []byte("\xff\xd8\xff\xd9"), "image/jpeg"},
		{"c.gif", []byte("GIF89a!"), "image/gif"},
		{"d.webp", []byte("RIFFabcdWEBPVP8 "), "image/webp"},
		{"e.heic", []byte("\x00\x00\x00\x10ftypheic0000"), "image/heic"},
		{"f.heif", []byte("\x00\x00\x00\x10ftypmif10000"), "image/heif"},
		{"g.pdf", []byte("%PDF-1.7\n"), "application/pdf"},
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, writeMediaTestFile(t, file.name, file.data))
	}
	parts, err := (&geminiClient{}).prepareImageInputs(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != len(files) {
		t.Fatalf("got %d parts, want %d", len(parts), len(files))
	}
	for i, part := range parts {
		if part.InlineData == nil || part.InlineData.MIMEType != files[i].mime || part.FileData != nil {
			t.Fatalf("part %d has unexpected format: %+v", i, part)
		}
		decoded, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil || string(decoded) != string(files[i].data) {
			t.Fatalf("part %d bytes changed", i)
		}
	}
}

func TestPrepareImageInputsRejectsInvalidMedia(t *testing.T) {
	for _, name := range []string{"bad.png", "bad.pdf", "bad.heic", "bad.webp"} {
		t.Run(name, func(t *testing.T) {
			path := writeMediaTestFile(t, name, []byte("fake media"))
			_, err := (&geminiClient{}).prepareImageInputs(context.Background(), []string{path})
			if err == nil || !strings.Contains(err.Error(), "not a valid") {
				t.Fatalf("wanted format error, got %v", err)
			}
		})
	}
	path := writeMediaTestFile(t, "bad.txt", []byte("hello"))
	if _, err := (&geminiClient{}).prepareImageInputs(context.Background(), []string{path}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("wanted unsupported extension error, got %v", err)
	}
}

func TestPrepareImageInputsUploadsWhenInlineBudgetExceeded(t *testing.T) {
	first := writeMediaTestFile(t, "first.pdf", []byte("%PDF-1.7\nfirst"))
	second := writeMediaTestFile(t, "large.pdf", []byte("%PDF-1.7\n"))
	if err := os.Truncate(second, maxInlineMediaBytes); err != nil {
		t.Fatal(err)
	}
	var uploaded atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/session")
		case "/upload/v1beta/files/session":
			uploaded.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"file":{"name":"files/abc123","uri":"gemini://files/abc123","mimeType":"application/pdf","state":"ACTIVE"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	parts, err := client.prepareImageInputs(context.Background(), []string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].InlineData == nil || parts[1].FileData == nil || uploaded.Load() != 1 {
		t.Fatalf("unexpected parts: %+v; uploaded=%d", parts, uploaded.Load())
	}
}

func TestUploadFileResumableProtocol(t *testing.T) {
	path := writeMediaTestFile(t, "batch.jsonl", []byte("{\"key\":\"value\"}\n"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			requests.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("x-goog-api-key") != "secret" || r.Header.Get("X-Goog-Upload-Protocol") != "resumable" || r.Header.Get("X-Goog-Upload-Command") != "start" || r.Header.Get("X-Goog-Upload-Header-Content-Type") != "application/jsonl" || r.Header.Get("X-Goog-Upload-Header-Content-Length") != "16" {
				t.Errorf("invalid upload start: %+v", r.Header)
			}
			var metadata struct {
				File struct {
					DisplayName string `json:"displayName"`
				} `json:"file"`
			}
			if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil || metadata.File.DisplayName != "batch.jsonl" {
				t.Errorf("invalid metadata: %+v, %v", metadata, err)
			}
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/session?upload_id=opaque")
		case "/upload/v1beta/files/session":
			requests.Add(1)
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != "{\"key\":\"value\"}\n" || r.Header.Get("x-goog-api-key") != "" || r.Header.Get("X-Goog-Upload-Offset") != "0" || r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" || r.Header.Get("Content-Type") != "application/jsonl" || r.ContentLength != int64(len(data)) {
				t.Errorf("invalid upload transfer: body=%q headers=%+v length=%d err=%v", data, r.Header, r.ContentLength, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"file":{"name":"files/abc123","uri":"https://generativelanguage.googleapis.com/v1beta/files/abc123","mimeType":"application/jsonl","state":"ACTIVE"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "secret", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	file, err := client.uploadFile(context.Background(), path, "application/jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "files/abc123" || file.MIMEType != "application/jsonl" || requests.Load() != 2 {
		t.Fatalf("unexpected upload result: %+v; requests=%d", file, requests.Load())
	}
}

func TestPrepareVideoPollsUntilActive(t *testing.T) {
	path := writeMediaTestFile(t, "clip.mp4", []byte("\x00\x00\x00\x18ftypmp42data"))
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/session")
		case "/upload/v1beta/files/session":
			_, _ = io.WriteString(w, `{"file":{"name":"files/abc123","uri":"gemini://files/abc123","mimeType":"video/mp4","state":"PROCESSING"}}`)
		case "/v1beta/files/abc123":
			polls.Add(1)
			_, _ = io.WriteString(w, `{"name":"files/abc123","uri":"gemini://files/abc123","mimeType":"video/mp4","state":"ACTIVE"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	parts, err := client.prepareImageInputs(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].FileData == nil || parts[0].FileData.FileURI != "gemini://files/abc123" || polls.Load() != 1 {
		t.Fatalf("unexpected video part: %+v; polls=%d", parts, polls.Load())
	}
}

func TestVideoPollingCancellationAndFailureRedaction(t *testing.T) {
	file := geminiFile{Name: "files/abc123", URI: "gemini://files/abc123", State: "PROCESSING"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"files/abc123","state":"PROCESSING"}`)
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "secret-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.waitForFile(ctx, file)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted deadline error, got %v", err)
	}
	file.State = "FAILED"
	file.Error.Message = "processing failed for secret-key"
	_, err = client.waitForFile(context.Background(), file)
	if err == nil || strings.Contains(err.Error(), "secret-key") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("wanted redacted failure, got %v", err)
	}
}

func TestUploadRejectsForeignUploadURL(t *testing.T) {
	path := writeMediaTestFile(t, "batch.jsonl", []byte("{}\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Goog-Upload-URL", "https://attacker.example/upload/v1beta/files/session?key=secret")
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "secret", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	_, err := client.uploadFile(context.Background(), path, "application/jsonl")
	if err == nil || !strings.Contains(err.Error(), "invalid upload URL") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("wanted safe upload URL error, got %v", err)
	}
}
