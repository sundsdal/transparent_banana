package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOmniVideoRequestAndURIDownload(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("missing API key on %s", r.URL.Path)
		}
		switch r.URL.RequestURI() {
		case "/v1beta/interactions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body["model"] != defaultVideoModel || body["background"] != false || body["stream"] != false || body["store"] != false {
				t.Errorf("unexpected request: %#v", body)
			}
			parts, ok := body["input"].([]any)
			if !ok || len(parts) != 2 {
				t.Errorf("input = %#v", body["input"])
				return
			}
			image := parts[0].(map[string]any)
			text := parts[1].(map[string]any)
			if image["type"] != "image" || image["data"] != "cGl4ZWxz" || image["mime_type"] != "image/png" || text["type"] != "text" || text["text"] != "animate" {
				t.Errorf("input parts = %#v", parts)
			}
			format := body["response_format"].(map[string]any)
			if format["type"] != "video" || format["aspect_ratio"] != "16:9" || format["resolution"] != "720p" || format["duration"] != "8s" || format["delivery"] != "uri" {
				t.Errorf("response format = %#v", format)
			}
			_, _ = io.WriteString(w, `{"status":"completed","steps":[{"type":"user_input","content":[{"type":"video","mime_type":"video/mp4","data":"d3Jvbmc="}]},{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://generativelanguage.googleapis.com/v1beta/files/file_123:download?alt=media"}]}]}`)
		case "/v1beta/files/file_123":
			_, _ = io.WriteString(w, `{"name":"files/file_123","state":"ACTIVE"}`)
		case "/v1beta/files/file_123:download?alt=media":
			_, _ = io.WriteString(w, "mp4 bytes")
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	video, err := client.generateVideo(context.Background(), defaultVideoModel, "animate", []imageData{{Data: []byte("pixels"), MIMEType: "image/png"}}, "16:9", "720p", "8s")
	if err != nil || string(video) != "mp4 bytes" {
		t.Fatalf("video = %q, %v", video, err)
	}
	want := []string{"POST /v1beta/interactions", "GET /v1beta/files/file_123", "GET /v1beta/files/file_123:download?alt=media"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("requests = %#v, want %#v", paths, want)
	}
}

func TestOmniTextOnlyInlineResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["input"] != "a waving cat" {
			t.Errorf("text input = %#v", body["input"])
		}
		format := body["response_format"].(map[string]any)
		if _, ok := format["duration"]; ok {
			t.Errorf("default duration must be omitted: %#v", format)
		}
		_, _ = io.WriteString(w, `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4; codecs=avc1","data":"bXA0"}]}]}`)
	}))
	defer server.Close()
	video, err := (&geminiClient{apiKey: "key", baseURL: server.URL, httpClient: server.Client()}).generateVideo(context.Background(), defaultVideoModel, "a waving cat", nil, "", "", "")
	if err != nil || string(video) != "mp4" {
		t.Fatalf("video = %q, %v", video, err)
	}
}

func TestOmniDownloadRedirectDoesNotForwardKey(t *testing.T) {
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "" {
			t.Error("API key forwarded to storage origin")
		}
		_, _ = io.WriteString(w, "redirected mp4")
	}))
	defer storage.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/v1beta/interactions":
			_, _ = io.WriteString(w, `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://generativelanguage.googleapis.com/v1beta/files/id:download?alt=media"}]}]}`)
		case "/v1beta/files/id":
			_, _ = io.WriteString(w, `{"state":"ACTIVE"}`)
		case "/v1beta/files/id:download?alt=media":
			http.Redirect(w, r, storage.URL+"/video", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	video, err := (&geminiClient{apiKey: "secret-key", baseURL: server.URL + "/v1beta", httpClient: storage.Client()}).generateVideo(context.Background(), defaultVideoModel, "prompt", nil, "", "", "")
	if err != nil || string(video) != "redirected mp4" {
		t.Fatalf("video = %q, %v", video, err)
	}
}

func TestOmniVideoErrorsAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name, response, want string
		status               int
	}{
		{"api error", `{"error":{"message":"quota exhausted: secret-key"}}`, "quota exhausted: [REDACTED]", 429},
		{"failed interaction", `{"status":"failed","error":{"message":"private reason secret-key"}}`, "private reason [REDACTED]", 200},
		{"text only", `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"no video for secret-key"}]}]}`, "no video for [REDACTED]", 200},
		{"wrong MIME", `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"image/png","data":"bXA0"}]}]}`, "did not contain an MP4", 200},
		{"invalid base64", `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"%%%"}]}]}`, "decode Gemini video data", 200},
		{"empty base64", `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":""}]}]}`, "did not contain an MP4", 200},
		{"invalid URI", `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://other.example/evil:download"}]}]}`, "invalid video file URI", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			_, err := (&geminiClient{apiKey: "secret-key", baseURL: server.URL, httpClient: server.Client()}).generateVideo(context.Background(), defaultVideoModel, "prompt", nil, "", "", "")
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "secret-key") {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/interactions" {
			_, _ = io.WriteString(w, `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://generativelanguage.googleapis.com/v1beta/files/waiting:download?alt=media"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"state":"PROCESSING"}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := (&geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}).generateVideo(ctx, defaultVideoModel, "prompt", nil, "", "", "")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("poll cancellation error = %v", err)
	}
}

func TestOmniFailedFileState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/interactions":
			_, _ = io.WriteString(w, `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://generativelanguage.googleapis.com/v1beta/files/failed:download?alt=media"}]}]}`)
		case "/v1beta/files/failed":
			_, _ = io.WriteString(w, `{"state":"FAILED","error":{"message":"processing failed: private-key"}}`)
		default:
			t.Errorf("unexpected download request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, err := (&geminiClient{apiKey: "private-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}).generateVideo(context.Background(), defaultVideoModel, "prompt", nil, "", "", "")
	if err == nil || !strings.Contains(err.Error(), "Gemini video file failed") || !strings.Contains(err.Error(), "[REDACTED]") || strings.Contains(err.Error(), "private-key") {
		t.Fatalf("file failure error = %v", err)
	}
}
