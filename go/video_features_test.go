package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAdvancedVideoArgs(t *testing.T) {
	o, err := parseArgs([]string{"-m", "omni", "--task", "reference_to_video", "--first-frame", "start.png", "--last-frame", "end.jpg", "--session", "turns.json", "--response-json", "response.json", "travel"})
	if err != nil || !o.video || o.task != "reference_to_video" || o.firstFrame != "start.png" || o.lastFrame != "end.jpg" || o.session != "turns.json" || o.responseJSON != "response.json" {
		t.Fatalf("parsed video options = %+v, %v", o, err)
	}
	for _, args := range [][]string{
		{"prompt", "--task", "edit"},
		{"prompt", "--first-frame", "a.png"},
		{"prompt", "--previous-interaction-id", "id"},
		{"prompt", "--video", "--task", "unknown"},
		{"prompt", "--video", "--last-frame", "end.png"},
		{"prompt", "--video", "--task", "text_to_video", "-i", "x.png"},
		{"prompt", "--video", "--session", "output.mp4"},
		{"prompt", "--video", "--response-json", "output.mp4"},
		{"prompt", "--video", "--session", "same.json", "--response-json", "same.json"},
		{"prompt", "--video", "--previous-interaction-id", " id "},
		{"prompt", "-m", "omni", "--thinking", "high"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted invalid args: %v", args)
		}
	}
}

func TestOutputPathAliasCollisions(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"prompt", "--video", "--response-json", filepath.Join(wd, "output.mp4")},
		{"prompt", "--video", "--session", filepath.Join(wd, "output.mp4")},
		{"prompt", "--video", "--session", "turns.json", "--response-json", filepath.Join(wd, "turns.json")},
		{"prompt", "--response-json", filepath.Join(wd, "output.png")},
		{"prompt", "--session", filepath.Join(wd, "output.png")},
		{"prompt", "--session", "turns.json", "--response-json", filepath.Join(wd, "turns.json")},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("accepted path alias collision: %v", args)
		}
	}
}

func TestAdvancedVideoRequestShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/interactions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["previous_interaction_id"] != "previous-1" || body["store"] != true || body["background"] != false || body["stream"] != false {
			t.Errorf("interaction settings = %#v", body)
		}
		generation, ok := body["generation_config"].(map[string]any)
		if !ok || generation["video_config"].(map[string]any)["task"] != "extend" {
			t.Errorf("generation config = %#v", generation)
		}
		parts, ok := body["input"].([]any)
		if !ok || len(parts) != 3 {
			t.Fatalf("input = %#v", body["input"])
		}
		video, image, prompt := parts[0].(map[string]any), parts[1].(map[string]any), parts[2].(map[string]any)
		if video["type"] != "video" || video["uri"] != "files/base" || image["type"] != "image" || image["data"] != "cGl4ZWxz" || prompt["text"] != "Continue the scene" {
			t.Errorf("parts = %#v", parts)
		}
		_, _ = io.WriteString(w, `{"id":"new-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"bXA0"}]}]}`)
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	result, err := client.generateVideoAdvanced(context.Background(), videoGenerationRequest{Model: defaultVideoModel, Prompt: "Continue the scene", Task: "extend", PreviousInteractionID: "previous-1", Store: true, Inputs: []omniInput{{Type: "video", URI: "files/base"}, {Type: "image", Data: "cGl4ZWxz", MIMEType: "image/png"}}})
	if err != nil || string(result.Video) != "mp4" || result.InteractionID != "new-2" || !bytes.Contains(result.RawResponse, []byte(`"new-2"`)) {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

type fakeAdvancedVideo struct {
	parts    []omniInput
	paths    [][]string
	requests []videoGenerationRequest
	result   videoGenerationResult
	err      error
}

func (f *fakeAdvancedVideo) prepareOmniInputs(_ context.Context, paths []string) ([]omniInput, error) {
	f.paths = append(f.paths, append([]string(nil), paths...))
	return f.parts, nil
}

func (f *fakeAdvancedVideo) generateVideoAdvanced(_ context.Context, req videoGenerationRequest) (videoGenerationResult, error) {
	f.requests = append(f.requests, req)
	return f.result, f.err
}

func TestVideoSessionCreateResumeAndFailure(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "turns.json")
	firstOutput := filepath.Join(dir, "first.mp4")
	secondOutput := filepath.Join(dir, "second.mp4")
	client := &fakeAdvancedVideo{result: videoGenerationResult{Video: []byte("first"), InteractionID: "turn-1", RawResponse: []byte(`{"id":"turn-1"}`)}}
	o := options{video: true, model: defaultVideoModel, prompt: "a cat waves", output: firstOutput, session: sessionPath, responseJSON: filepath.Join(dir, "response.json")}
	var log bytes.Buffer
	if err := runAdvancedVideo(context.Background(), o, client, &log); err != nil {
		t.Fatal(err)
	}
	if !client.requests[0].Store || client.requests[0].PreviousInteractionID != "" || !strings.Contains(log.String(), "Interaction ID: turn-1") {
		t.Fatalf("initial request = %+v; output = %q", client.requests[0], log.String())
	}
	info, err := os.Stat(sessionPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session permissions = %v, %v", info, err)
	}
	client.result = videoGenerationResult{Video: []byte("second"), InteractionID: "turn-2", RawResponse: []byte(`{"id":"turn-2"}`)}
	o.output = secondOutput
	o.prompt = "make the cat orange"
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err != nil {
		t.Fatal(err)
	}
	if client.requests[1].PreviousInteractionID != "turn-1" || !client.requests[1].Store {
		t.Fatalf("resumed request = %+v", client.requests[1])
	}
	session, err := loadVideoSession(sessionPath, defaultVideoModel)
	if err != nil || session.LastInteractionID != "turn-2" {
		t.Fatalf("session = %+v, %v", session, err)
	}
	client.err = errors.New("generation failed")
	o.output = filepath.Join(dir, "failed.mp4")
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err == nil {
		t.Fatal("generation failure was ignored")
	}
	session, err = loadVideoSession(sessionPath, defaultVideoModel)
	if err != nil || session.LastInteractionID != "turn-2" {
		t.Fatalf("failed turn changed session: %+v, %v", session, err)
	}
}

func TestVideoSessionRejectsWrongKindAndMismatchBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	client := &fakeAdvancedVideo{result: videoGenerationResult{Video: []byte("mp4"), InteractionID: "id"}}
	o := options{video: true, model: defaultVideoModel, prompt: "prompt", output: filepath.Join(dir, "out.mp4"), session: path}
	if err := os.WriteFile(path, []byte(`{"model":"gemini-nano-banana-2.1","contents":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err == nil || len(client.requests) != 0 {
		t.Fatalf("image session should be rejected before request: %v", err)
	}
	if err := saveVideoSession(path, videoSession{Kind: "video", Version: 1, Model: defaultVideoModel, LastInteractionID: "stored-id"}); err != nil {
		t.Fatal(err)
	}
	o.previousInteractionID = "stored-id"
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err != nil {
		t.Fatalf("matching explicit interaction ID should be accepted: %v", err)
	}
	client.requests = nil
	if err := saveVideoSession(path, videoSession{Kind: "video", Version: 1, Model: defaultVideoModel, LastInteractionID: "stored-id"}); err != nil {
		t.Fatal(err)
	}
	o.previousInteractionID = "different-id"
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err == nil || len(client.requests) != 0 {
		t.Fatalf("mismatched interaction ID should be rejected before request: %v", err)
	}
	if _, err := loadImageSession(path, defaultModel); err == nil || !strings.Contains(err.Error(), "video session") {
		t.Fatalf("image loader accepted video session: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"last_interaction_id":"id"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVideoSession(path, defaultVideoModel); err == nil {
		t.Fatal("video session without kind/version/model was accepted")
	}
}

func TestVideoSessionExtendWithUploadedReferences(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "turns.json")
	if err := saveVideoSession(sessionPath, videoSession{Kind: "video", Version: 1, Model: defaultVideoModel, LastInteractionID: "generated-1"}); err != nil {
		t.Fatal(err)
	}
	client := &fakeAdvancedVideo{
		parts:  []omniInput{{Type: "video", URI: "files/reference-video"}, {Type: "image", Data: "cGl4ZWxz", MIMEType: "image/png"}},
		result: videoGenerationResult{Video: []byte("extended"), InteractionID: "generated-2"},
	}
	o := options{video: true, model: defaultVideoModel, prompt: "continue with the character", output: filepath.Join(dir, "out.mp4"), session: sessionPath, input: "reference.mp4,character.png", task: "extend"}
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err != nil {
		t.Fatal(err)
	}
	request := client.requests[0]
	if request.PreviousInteractionID != "generated-1" || request.Prompt != "[# References <VIDEO_REF_0>@Video1 <IMAGE_REF_0>@Image1] continue with the character" {
		t.Fatalf("uploaded refs should not replace previous source: %+v", request)
	}
}

func TestVideoSessionDoesNotAdvanceAfterSaveFailure(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "turns.json")
	if err := saveVideoSession(sessionPath, videoSession{Kind: "video", Version: 1, Model: defaultVideoModel, LastInteractionID: "original"}); err != nil {
		t.Fatal(err)
	}
	client := &fakeAdvancedVideo{result: videoGenerationResult{Video: []byte("mp4"), InteractionID: "new"}}
	o := options{video: true, model: defaultVideoModel, prompt: "change it", session: sessionPath, output: filepath.Join(dir, "missing", "out.mp4")}
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err == nil {
		t.Fatal("expected output save failure")
	}
	session, err := loadVideoSession(sessionPath, defaultVideoModel)
	if err != nil || session.LastInteractionID != "original" {
		t.Fatalf("failed save advanced session: %+v, %v", session, err)
	}
}

func TestVideoSessionDoesNotAdvanceAfterDownloadFailure(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "turns.json")
	if err := saveVideoSession(sessionPath, videoSession{Kind: "video", Version: 1, Model: defaultVideoModel, LastInteractionID: "original"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/interactions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["previous_interaction_id"] != "original" || body["store"] != true {
				t.Errorf("continuation request = %#v", body)
			}
			_, _ = io.WriteString(w, `{"id":"new","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://generativelanguage.googleapis.com/v1beta/files/bad:download?alt=media"}]}]}`)
		case "/v1beta/files/bad":
			_, _ = io.WriteString(w, `{"state":"FAILED","error":{"message":"processing failed"}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	o := options{video: true, model: defaultVideoModel, prompt: "change it", session: sessionPath, output: filepath.Join(dir, "out.mp4")}
	if err := runAdvancedVideo(context.Background(), o, client, io.Discard); err == nil || !strings.Contains(err.Error(), "video file failed") {
		t.Fatalf("expected download failure, got %v", err)
	}
	session, err := loadVideoSession(sessionPath, defaultVideoModel)
	if err != nil || session.LastInteractionID != "original" {
		t.Fatalf("failed download advanced session: %+v, %v", session, err)
	}
}

func TestVideoRolePromptNumbersEachMediaType(t *testing.T) {
	parts := []omniInput{{Type: "image"}, {Type: "image"}, {Type: "video"}, {Type: "image"}, {Type: "video"}}
	got := videoRolePrompt("animate", "reference_to_video", "", parts, 2)
	want := "[# Sources <FIRST_FRAME>@Image1 <LAST_FRAME>@Image2] [# References <VIDEO_REF_0>@Video1 <IMAGE_REF_0>@Image3 <VIDEO_REF_1>@Video2] animate"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	if got := videoRolePrompt("animate", "", "", parts, 0); got != "animate" {
		t.Fatalf("plain input changed prompt inference: %q", got)
	}
	got = videoRolePrompt("continue", "extend", "previous-id", []omniInput{{Type: "video"}, {Type: "image"}}, 0)
	if got != "[# References <VIDEO_REF_0>@Video1 <IMAGE_REF_0>@Image1] continue" {
		t.Fatalf("previous video source was overridden by upload: %q", got)
	}
}
