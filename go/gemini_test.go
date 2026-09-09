package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGeminiGenerateSendsExpectedRequestAndParsesImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/models/gemini-3.1-flash-image:generateContent" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("API key header = %q", got)
			http.Error(w, "missing API key", http.StatusBadRequest)
			return
		}
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		parts := request.Contents[0].Parts
		if len(parts) != 2 || parts[0].Text != "draw a banana" || parts[1].InlineData == nil {
			t.Errorf("unexpected parts: %#v", parts)
			http.Error(w, "unexpected parts", http.StatusBadRequest)
			return
		}
		if parts[1].InlineData.Data != "cGl4ZWxz" || parts[1].InlineData.MIMEType != "image/png" {
			t.Errorf("unexpected inline data: %#v", parts[1].InlineData)
			http.Error(w, "unexpected inline data", http.StatusBadRequest)
			return
		}
		if got := request.GenerationConfig.ResponseModalities; len(got) != 1 || got[0] != "IMAGE" {
			t.Errorf("modalities = %#v", got)
			http.Error(w, "unexpected modalities", http.StatusBadRequest)
			return
		}
		if request.GenerationConfig.ImageConfig == nil || request.GenerationConfig.ImageConfig.AspectRatio != "16:9" {
			t.Errorf("image config = %#v", request.GenerationConfig.ImageConfig)
			http.Error(w, "unexpected image config", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"thought":true,"inlineData":{"mimeType":"image/png","data":"d3Jvbmc="}},{"inlineData":{"mimeType":"application/json","data":"e30="}},{"inlineData":{"mimeType":"image/jpeg","data":"aW1hZ2U="}}]}}]}`))
	}))
	defer server.Close()

	client := geminiClient{apiKey: "test-key", baseURL: server.URL, httpClient: server.Client()}
	got, err := client.generate(context.Background(), "gemini-3.1-flash-image", "draw a banana", []imageData{{Data: []byte("pixels"), MIMEType: "image/png"}}, "16:9")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != "image" || got.MIMEType != "image/jpeg" {
		t.Fatalf("image = %#v", got)
	}
}

func TestGeminiGenerateOmitsImageConfigWithoutAspect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if request.GenerationConfig.ImageConfig != nil {
			t.Errorf("image config = %#v", request.GenerationConfig.ImageConfig)
			http.Error(w, "unexpected image config", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="}}]}}]}`))
	}))
	defer server.Close()
	_, err := (&geminiClient{apiKey: "key", baseURL: server.URL, httpClient: server.Client()}).generate(context.Background(), "model", "prompt", nil, "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestGeminiGenerateStatusErrorIsUsefulAndDoesNotExposeKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted: private-api-key"}}`))
	}))
	defer server.Close()
	key := "private-api-key"
	_, err := (&geminiClient{apiKey: key, baseURL: server.URL, httpClient: server.Client()}).generate(context.Background(), "model", "prompt", nil, "")
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unsafe error = %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("redacted error = %v", err)
	}
}

func TestGeminiGenerateHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := (&geminiClient{apiKey: "key", baseURL: server.URL, httpClient: server.Client()}).generate(ctx, "model", "prompt", nil, "")
		errCh <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errCh:
		close(release)
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("generate did not honor context cancellation")
	}
}

func TestGeminiGenerateReportsSafeNoImageDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "prompt blocked without candidates",
			body: `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"prompt violates policy"}}`,
			want: []string{"prompt blocked: SAFETY", "prompt feedback: prompt violates policy"},
		},
		{
			name: "candidate safety rejection",
			body: `{"candidates":[{"finishReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH","blocked":true}],"content":{"parts":[]}}]}`,
			want: []string{"candidate finish reason: SAFETY", "HARM_CATEGORY_DANGEROUS_CONTENT (HIGH) blocked"},
		},
		{
			name: "text only response redacts key and ignores thought",
			body: `{"candidates":[{"content":{"parts":[{"thought":true,"text":"private-api-key hidden reasoning"},{"text":"Cannot create that image: private-api-key"}]}}]}`,
			want: []string{"model text: \"Cannot create that image: [REDACTED]\""},
		},
		{
			name: "empty response remains generic",
			body: `{"candidates":[]}`,
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := (&geminiClient{apiKey: "private-api-key", baseURL: server.URL, httpClient: server.Client()}).generate(context.Background(), "model", "prompt", nil, "")
			if err == nil {
				t.Fatal("generate unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), "private-api-key") {
				t.Fatalf("unredacted diagnostic: %v", err)
			}
			if strings.Contains(err.Error(), "hidden reasoning") {
				t.Fatalf("thought text leaked into diagnostic: %v", err)
			}
			if len(test.want) == 0 {
				if got, want := err.Error(), "Gemini response did not contain an image"; got != want {
					t.Fatalf("error = %q, want %q", got, want)
				}
				return
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, missing %q", err, want)
				}
			}
		})
	}
}

func TestGeminiDiagnosticsRedactsBeforeTruncating(t *testing.T) {
	key := "unique-secret-key"
	response := geminiResponse{Candidates: []geminiCandidate{{}}}
	response.Candidates[0].Content.Parts = []geminiResponsePart{{
		Text: strings.Repeat("a", maxGeminiDiagnosticLength-5) + key,
	}}
	diagnostic := (&geminiClient{apiKey: key}).responseDiagnostics(response)
	if strings.Contains(diagnostic, "unique") || strings.Contains(diagnostic, key) {
		t.Fatalf("credential fragment leaked at truncation boundary: %q", diagnostic)
	}
}

func TestGeminiDiagnosticsAreUTF8SafeAndIncludeAllSafetyRatings(t *testing.T) {
	if got := boundedDiagnostic(strings.Repeat("あ", 400)); !utf8.ValidString(got) {
		t.Fatalf("truncated diagnostic is invalid UTF-8: %q", got)
	}
	ratings := []geminiSafetyRating{
		{Category: "ONE"}, {Category: "TWO"}, {Category: "THREE"}, {Category: "FOUR"}, {Category: "FIVE"},
	}
	if got := safetyRatingsDiagnostic(ratings); !strings.Contains(got, "FIVE") {
		t.Fatalf("fifth safety rating omitted: %q", got)
	}
}
