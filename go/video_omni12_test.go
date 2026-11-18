package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOmni12VertexUpscale(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		t.Run(fmt.Sprintf("stateful=%t", stateful), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/interactions" ||
					r.Header.Get("Authorization") != "Bearer cloud-secret" || r.Header.Get("x-goog-api-key") != "" ||
					r.Header.Get("x-goog-user-project") != "test-project" ||
					r.Header.Get("X-Vertex-AI-LLM-Request-Type") != "shared" {
					t.Errorf("unexpected Cloud request %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					Model                 string                `json:"model"`
					Input                 json.RawMessage       `json:"input"`
					ResponseFormat        []omniVideoFormat     `json:"response_format"`
					GenerationConfig      *omniGenerationConfig `json:"generation_config"`
					PreviousInteractionID string                `json:"previous_interaction_id"`
					Labels                map[string]string     `json:"labels"`
					Store                 bool                  `json:"store"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode Cloud request: %v", err)
				}
				if body.Model != defaultVideoModel || body.Store != stateful || body.Labels["team"] != "media" ||
					len(body.ResponseFormat) != 1 || body.ResponseFormat[0].Delivery != "inline" || body.ResponseFormat[0].Resolution != "1080p" ||
					body.GenerationConfig == nil || body.GenerationConfig.VideoConfig == nil ||
					body.GenerationConfig.VideoConfig.Task != "upscale" || body.GenerationConfig.Seed == nil || *body.GenerationConfig.Seed != 0 {
					t.Errorf("unexpected Cloud request payload: %+v", body)
				}
				if stateful {
					var steps []omniStep
					err := json.Unmarshal(body.Input, &steps)
					if err != nil || body.PreviousInteractionID != "previous-id" || len(steps) != 1 ||
						steps[0].Type != "user_input" || len(steps[0].Content) != 1 || steps[0].Content[0].Type != "text" || steps[0].Content[0].Text == "" {
						t.Errorf("unexpected previous-turn upscale input: %s (%v)", body.Input, err)
					}
				} else {
					var parts []omniInput
					err := json.Unmarshal(body.Input, &parts)
					if err != nil || body.PreviousInteractionID != "" || len(parts) != 1 ||
						parts[0].Type != "video" || parts[0].URI != "gs://test-bucket/draft.mp4" || parts[0].MIMEType != "video/mp4" {
						t.Errorf("unexpected uploaded upscale input: %s (%v)", body.Input, err)
					}
				}
				fmt.Fprint(w, `{"id":"upscale-id","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"bXA0"}]}]}`)
			}))
			defer server.Close()

			output := filepath.Join(t.TempDir(), "out.mp4")
			args := []string{"--vertex-project", "test-project", "--vertex-traffic", "shared", "--task", "upscale", "--resolution", "1080p", "--seed", "0", "--label", "team=media", "-o", output}
			if stateful {
				args = append(args, "--previous-interaction-id", "previous-id")
			} else {
				args = append(args, "-i", "gs://test-bucket/draft.mp4")
			}
			o, err := parseArgs(args)
			if err != nil {
				t.Fatal(err)
			}
			client := &geminiClient{baseURL: server.URL, vertexProject: o.vertexProject, vertexToken: "cloud-secret", vertexTraffic: o.vertexTraffic, httpClient: server.Client()}
			if err := run(context.Background(), o, client, io.Discard); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil || string(data) != "mp4" {
				t.Fatalf("upscaled output = %q, %v", data, err)
			}
		})
	}
}

func TestOmni12BackgroundLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/interactions":
			var request omniRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Background || !request.Store || request.Stream ||
				request.ResponseFormat.Duration != "15s" || request.Model != defaultVideoModel {
				t.Errorf("unexpected background request: %+v, %v", request, err)
			}
			fmt.Fprint(w, `{"id":"job-id","status":"in_progress"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/interactions/job-id":
			fmt.Fprint(w, `{"id":"job-id","status":"completed","output_video":{"type":"video","mime_type":"video/mp4","data":"bXA0"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/interactions/other-id/cancel":
			fmt.Fprint(w, `{"id":"other-id","status":"cancelled"}`)
		default:
			t.Errorf("unexpected background request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{baseURL: server.URL, apiKey: "gemini-key", httpClient: server.Client()}
	output := filepath.Join(t.TempDir(), "out.mp4")
	for _, step := range []struct {
		args   []string
		status string
	}{
		{[]string{"--video", "--background", "--duration", "15s", "paper boat"}, "Interaction ID: job-id"},
		{[]string{"--get-interaction-id", "job-id", "-o", output}, "Video saved"},
		{[]string{"--cancel-interaction-id", "other-id"}, "Status: cancelled"},
	} {
		o, err := parseArgs(step.args)
		if err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		if err := run(context.Background(), o, client, &log); err != nil || !strings.Contains(log.String(), step.status) {
			t.Fatalf("run %v: %v, %s", step.args, err, log.String())
		}
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "mp4" {
		t.Fatalf("video output = %q, %v", data, err)
	}
}

func TestOmni12StreamLargeVideoEvent(t *testing.T) {
	video := strings.Repeat("x", 65_537)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("stream") != "true" {
			t.Error("retrieval did not request a stream")
		}
		if r.Method == http.MethodPost {
			var request omniRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream || !request.Store || request.Background {
				t.Errorf("unexpected streaming request: %+v, %v", request, err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"stream-id\",\"status\":\"in_progress\"}}\r\n\r\n")
		fmt.Fprint(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"thought_summary\",\"content\":{\"text\":\"Working\"}}}\n\n")
		fmt.Fprintf(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"video\",\"mime_type\":\"video/mp4\",\"data\":%q}}\n\n", base64.StdEncoding.EncodeToString([]byte(video)))
		fmt.Fprint(w, "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"stream-id\",\"status\":\"completed\"}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	client := &geminiClient{baseURL: server.URL, apiKey: "gemini-key", httpClient: server.Client()}
	for _, id := range []string{"", "stream-id"} {
		var progress []string
		result, err := client.generateVideoAdvanced(context.Background(), videoGenerationRequest{
			Model: defaultVideoModel, Prompt: "a cloud", Store: true, CaptureResponse: true, GetInteractionID: id,
			StreamProgress: func(message string) { progress = append(progress, message) },
		})
		if err != nil || result.InteractionID != "stream-id" || string(result.Video) != video ||
			!json.Valid(result.RawResponse) || len(progress) != 2 || progress[1] != "Working" {
			t.Fatalf("stream id %q: MP4 size %d, progress %v, error %v", id, len(result.Video), progress, err)
		}
	}
}

func TestOmni12VertexStorageDelivery(t *testing.T) {
	client := &geminiClient{
		apiKey: "developer-key", vertexProject: "test-project", vertexToken: "cloud-token",
		httpClient: &http.Client{Transport: videoRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			var response string
			if r.Header.Get("Authorization") != "Bearer cloud-token" || r.Header.Get("x-goog-api-key") != "" {
				t.Errorf("wrong credentials for Cloud request to %s", r.URL.Host)
			}
			switch r.URL.Host {
			case "aiplatform.googleapis.com":
				if r.Method != http.MethodPost || r.URL.Path != "/v1beta1/projects/test-project/locations/global/interactions" {
					t.Errorf("unexpected Vertex endpoint: %s %s", r.Method, r.URL.Path)
				}
				var request struct {
					ResponseFormat []omniVideoFormat `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
					len(request.ResponseFormat) != 1 || request.ResponseFormat[0].GCSURI != "gs://video-bucket/renders/" ||
					request.ResponseFormat[0].Delivery != "uri" || request.ResponseFormat[0].Duration != "15s" {
					t.Errorf("unexpected Vertex response format: %+v, %v", request, err)
				}
				response = `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"gs://video-bucket/renders/clip%20one.mp4"}]}]}`
			case "storage.googleapis.com":
				if r.Method != http.MethodGet || r.URL.EscapedPath() != "/storage/v1/b/video-bucket/o/renders%2Fclip%20one.mp4" ||
					r.URL.Query().Get("alt") != "media" {
					t.Errorf("unexpected Cloud Storage download endpoint: %s %s", r.Method, r.URL)
				}
				response = "mp4"
			default:
				t.Fatalf("unexpected request to %s", r.URL.Host)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
		})},
	}
	output := filepath.Join(t.TempDir(), "out.mp4")
	o, err := parseArgs([]string{"--vertex-project", "test-project", "--duration", "15", "--gcs-output", "gs://video-bucket/renders/", "-o", output, "kite flying"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), o, client, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "mp4" {
		t.Fatalf("Cloud Storage output = %q, %v", data, err)
	}
}
