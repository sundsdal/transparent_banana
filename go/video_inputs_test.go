package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrepareOmniInputsInlineImagesPreserveOrder(t *testing.T) {
	files := []struct {
		name string
		data []byte
		mime string
	}{
		{"a.png", []byte("\x89PNG\r\n\x1a\nimage"), "image/png"},
		{"b.jpg", []byte("\xff\xd8\xffimage"), "image/jpeg"},
		{"c.gif", []byte("GIF89aimage"), "image/gif"},
		{"d.webp", []byte("RIFFabcdWEBPimage"), "image/webp"},
		{"e.heic", []byte("\x00\x00\x00\x10ftypheicimage"), "image/heic"},
		{"f.heif", []byte("\x00\x00\x00\x10ftypmif1image"), "image/heif"},
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, writeMediaTestFile(t, file.name, file.data))
	}
	parts, err := (&geminiClient{}).prepareOmniInputs(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != len(files) {
		t.Fatalf("got %d parts, want %d", len(parts), len(files))
	}
	for i, part := range parts {
		data, err := base64.StdEncoding.DecodeString(part.Data)
		if err != nil || part.Type != "image" || part.MIMEType != files[i].mime || part.URI != "" || string(data) != string(files[i].data) {
			t.Fatalf("unexpected part %d: %+v, decoded=%q, err=%v", i, part, data, err)
		}
	}
}

func TestPrepareOmniInputsVideoUploadAndActivePoll(t *testing.T) {
	image := writeMediaTestFile(t, "frame.png", []byte("\x89PNG\r\n\x1a\nimage"))
	video := writeMediaTestFile(t, "source.mp4", []byte("\x00\x00\x00\x18ftypmp42data"))
	var starts, transfers, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			starts.Add(1)
			if r.Header.Get("X-Goog-Upload-Header-Content-Type") != "video/mp4" {
				t.Errorf("unexpected upload MIME: %s", r.Header.Get("X-Goog-Upload-Header-Content-Type"))
			}
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/session")
		case "/upload/v1beta/files/session":
			transfers.Add(1)
			data, _ := io.ReadAll(r.Body)
			if string(data) != "\x00\x00\x00\x18ftypmp42data" {
				t.Errorf("unexpected video upload bytes: %q", data)
			}
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
	client := &geminiClient{apiKey: "secret", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	parts, err := client.prepareOmniInputs(context.Background(), []string{image, video})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Type != "image" || parts[0].Data == "" || parts[1].Type != "video" || parts[1].URI != "gemini://files/abc123" || parts[1].Data != "" || parts[1].MIMEType != "video/mp4" {
		t.Fatalf("unexpected Omni parts: %+v", parts)
	}
	if starts.Load() != 1 || transfers.Load() != 1 || polls.Load() != 1 {
		t.Fatalf("upload requests: starts=%d transfers=%d polls=%d", starts.Load(), transfers.Load(), polls.Load())
	}
	encoded, err := json.Marshal(parts)
	if err != nil || !strings.Contains(string(encoded), `"uri":"gemini://files/abc123"`) {
		t.Fatalf("video URI missing from request JSON: %s, %v", encoded, err)
	}
}

func TestPrepareOmniInputsPreflightsBeforeUpload(t *testing.T) {
	video := writeMediaTestFile(t, "source.mp4", []byte("\x00\x00\x00\x18ftypmp42data"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected upload", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	invalid := []string{
		writeMediaTestFile(t, "document.pdf", []byte("%PDF-1.7\n")),
		writeMediaTestFile(t, "bad.png", []byte("not a PNG")),
		writeMediaTestFile(t, "empty.mp4", nil),
		writeMediaTestFile(t, "audio.mp3", []byte("audio")),
		filepath.Join(t.TempDir(), "missing.mp4"),
		t.TempDir(),
	}
	for _, path := range invalid {
		if _, err := client.prepareOmniInputs(context.Background(), []string{video, path}); err == nil {
			t.Errorf("expected validation failure for %q", path)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("validation failure caused %d upload requests", requests.Load())
	}
}

func TestPrepareOmniInputsSharesInlineBudget(t *testing.T) {
	first := writeMediaTestFile(t, "first.png", []byte("\x89PNG\r\n\x1a\n"))
	second := writeMediaTestFile(t, "second.jpg", []byte("\xff\xd8\xffx"))
	if err := os.Truncate(first, maxInlineMediaBytes-3); err != nil {
		t.Fatal(err)
	}
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/session")
		case "/upload/v1beta/files/session":
			uploads.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"file":{"name":"files/abc123","uri":"gemini://files/abc123","mimeType":"image/jpeg","state":"ACTIVE"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	parts, err := client.prepareOmniInputs(context.Background(), []string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Data == "" || parts[0].URI != "" || parts[1].Data != "" || parts[1].URI != "gemini://files/abc123" || uploads.Load() != 1 {
		t.Fatalf("unexpected inline budget handling: %+v, uploads=%d", parts, uploads.Load())
	}
}
