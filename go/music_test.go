package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var testMP3 = []byte("ID3\x03\x00\x00\x00\x00\x00\x00fake mp3 frames")

func testWAV() []byte {
	samples := make([]byte, 48000*4) // one second of 48 kHz stereo
	return append(wavHeader(audioFormat{encodingL16, 48000, 2}, int64(len(samples))), samples...)
}

func TestParseMusicArgs(t *testing.T) {
	o, err := parseMusicArgs([]string{"a calm piano piece"})
	if err != nil || o.prompt != "a calm piano piece" || o.model != "lyria-3.5" || o.output != "output.mp3" || o.wav {
		t.Fatalf("defaults: %+v, %v", o, err)
	}
	o, err = parseMusicArgs([]string{"-m", "clip", "jingle", "-o", "j.WAV", "--instrumental", "-i", "a.png,b.jpg", "--input=c.webp"})
	if err != nil || o.model != "lyria-3-clip-preview" || !o.wav || !o.instrumental || !reflect.DeepEqual(o.inputs, []string{"a.png", "b.jpg", "c.webp"}) {
		t.Fatalf("clip: %+v, %v", o, err)
	}
	o, err = parseMusicArgs([]string{"song", "--format", "wav"})
	if err != nil || o.output != "output.wav" || !o.wav {
		t.Fatalf("format sets the default output: %+v, %v", o, err)
	}
	o, err = parseMusicArgs([]string{"song", "-o", "-", "--format=WAV"})
	if err != nil || o.output != "-" || !o.wav {
		t.Fatalf("stdout wav: %+v, %v", o, err)
	}
	if o, err = parseMusicArgs([]string{"song", "-o", "-"}); err != nil || o.wav {
		t.Fatalf("stdout defaults to mp3: %+v, %v", o, err)
	}
	if o, err = parseMusicArgs([]string{"-f", "-", "--lyrics", "words.txt", "-m", "lyria-3-pro-preview"}); err != nil || o.file != "-" || o.lyrics != "words.txt" || o.model != "lyria-3-pro-preview" {
		t.Fatalf("file and lyrics: %+v, %v", o, err)
	}
	if o, err = parseMusicArgs([]string{"--lyrics", "words.txt"}); err != nil || o.prompt != "" {
		t.Fatalf("lyrics alone: %+v, %v", o, err)
	}
	if o, err = parseMusicArgs([]string{"-h"}); err != nil || !o.help {
		t.Fatal("help should not need a prompt")
	}
	for _, args := range [][]string{
		nil,
		{"a", "b"},
		{"  "},
		{"--instrumental"},
		{"song", "-f", "p.txt"},
		{"song", "-o", "x.ogg"},
		{"song", "-o", "x.mp3", "--format", "wav"},
		{"song", "--format", "flac"},
		{"song", "--bogus"},
		{"song", "-m", "models/lyria"},
		{"song", "--instrumental", "--lyrics", "words.txt"},
		{"-f", "-", "--lyrics", "-"},
		{"song", "--play", "-o", "-"},
		{"song", "-i", "a.png,,b.png"},
		{"song", "-i", "1.png,2.png,3.png,4.png,5.png,6.png,7.png,8.png,9.png,10.png,11.png"},
		{"-f", "song.mp3", "-o", "song.mp3"},
		{"song", "--lyrics", "take-lyrics.txt", "-o", "take.mp3"},
		{"song", "-i", "cover.png", "--response-json", "cover.png"},
		{"song", "-o", "take.mp3", "--response-json", "take.mp3"},
		{"song", "-o", "take.mp3", "--response-json", "./take-lyrics.txt"},
	} {
		if _, err := parseMusicArgs(args); err == nil {
			t.Errorf("parseMusicArgs(%q) should fail", args)
		}
	}
}

func TestComposeMusicPrompt(t *testing.T) {
	for _, test := range []struct {
		prompt, lyrics string
		instrumental   bool
		want           string
	}{
		{" jazz trio ", "", false, "jazz trio"},
		{"jazz trio", "", true, "jazz trio\n\nInstrumental only, no vocals."},
		{"indie pop", "[Verse 1]\nHello\n", false, "indie pop\n\nUse the following lyrics:\n\n[Verse 1]\nHello"},
		{"", "[Chorus]\nLa la", false, "Create a song with the following lyrics:\n\n[Chorus]\nLa la"},
	} {
		if got := composeMusicPrompt(test.prompt, test.lyrics, test.instrumental); got != test.want {
			t.Errorf("composeMusicPrompt(%q, %q, %v) = %q, want %q", test.prompt, test.lyrics, test.instrumental, got, test.want)
		}
	}
}

func TestMusicPayload(t *testing.T) {
	decode := func(request musicRequest) map[string]any {
		t.Helper()
		payload, err := request.payload()
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	body := decode(musicRequest{Model: "lyria-3.5", Prompt: "piano"})
	if !reflect.DeepEqual(body, map[string]any{"model": "lyria-3.5", "input": "piano", "store": false}) {
		t.Fatalf("text payload = %v", body)
	}
	body = decode(musicRequest{Model: "lyria-3.5", Prompt: "piano", WAV: true, Images: []imageData{{Data: []byte("png"), MIMEType: "image/png"}}})
	wantInput := []any{
		map[string]any{"type": "text", "text": "piano"},
		map[string]any{"type": "image", "mime_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("png"))},
	}
	if !reflect.DeepEqual(body["input"], wantInput) {
		t.Fatalf("image input = %v", body["input"])
	}
	if !reflect.DeepEqual(body["response_format"], map[string]any{"type": "audio", "mime_type": "audio/wav"}) {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	for _, request := range []musicRequest{
		{Prompt: "piano"},
		{Model: "lyria-3.5", Prompt: " "},
		{Model: "lyria-3.5", Prompt: "piano", Images: make([]imageData, 11)},
		{Model: "lyria-3.5", Prompt: "piano", Images: []imageData{{Data: []byte("x"), MIMEType: "video/mp4"}}},
	} {
		if _, err := request.payload(); err == nil {
			t.Errorf("payload(%+v) should fail", request)
		}
	}
}

func TestMusicAudioExt(t *testing.T) {
	for _, test := range []struct {
		mime string
		data []byte
		want string
	}{
		{"audio/mpeg", testMP3, ".mp3"},
		{"audio/wav", testWAV(), ".wav"},
		{"audio/wav", testMP3, ".mp3"}, // the bytes win over the label
		{"", []byte{0xFF, 0xFB, 0x90, 0x00}, ".mp3"},
		{"audio/mp3; rate=48000", []byte("opaque"), ".mp3"},
		{"audio/x-wav", []byte("opaque"), ".wav"},
		{"audio/ogg", []byte("OggS"), ""},
	} {
		got, err := musicAudioExt(test.mime, test.data)
		if got != test.want || (err == nil) != (test.want != "") {
			t.Errorf("musicAudioExt(%q) = %q, %v; want %q", test.mime, got, err, test.want)
		}
	}
}

// musicServer answers with the given model_output content blocks, one step
// each, and records request bodies.
func musicServer(t *testing.T, requests *[]map[string]any, content func(body map[string]any) []string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/interactions" || r.Header.Get("x-goog-api-key") != "test-key" || r.Header.Get("Api-Revision") != "2026-05-20" {
			t.Errorf("unexpected request %s %s %v", r.Method, r.URL, r.Header)
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		*requests = append(*requests, body)
		var steps []string
		for _, block := range content(body) {
			steps = append(steps, `{"type":"model_output","content":[`+block+`]}`)
		}
		fmt.Fprintf(w, `{"status":"completed","object":"interaction","usage":{"total_input_tokens":14,"total_output_tokens":262},"steps":[%s]}`, strings.Join(steps, ","))
	}))
	t.Cleanup(server.Close)
	return server
}

func audioBlock(mimeType string, data []byte) string {
	return fmt.Sprintf(`{"type":"audio","mime_type":%q,"data":%q}`, mimeType, base64.StdEncoding.EncodeToString(data))
}

func textBlock(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"type":"text","text":` + string(encoded) + `}`
}

func TestRunMusic(t *testing.T) {
	dir := t.TempDir()
	var requests []map[string]any
	lyrics := "[0.0:] MORNING, NOON, OR LATE AT NIGHT\n[3.7:] BANANA!"
	server := musicServer(t, &requests, func(body map[string]any) []string {
		if format, ok := body["response_format"].(map[string]any); ok && format["mime_type"] == "audio/wav" {
			return []string{textBlock(instrumentalMarker), audioBlock("audio/wav", testWAV())}
		}
		return []string{textBlock(lyrics), audioBlock("audio/mpeg", testMP3)}
	})
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL, httpClient: server.Client()}

	words := filepath.Join(dir, "words.txt")
	cover := filepath.Join(dir, "cover.png")
	if err := os.WriteFile(words, []byte("[Verse 1]\nBananas at dawn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cover, []byte("\x89PNG\r\n\x1a\nrest"), 0o644); err != nil {
		t.Fatal(err)
	}
	song := filepath.Join(dir, "song.mp3")
	raw := filepath.Join(dir, "raw.json")
	o, err := parseMusicArgs([]string{"sunny pop", "--lyrics", words, "-i", cover, "-o", song, "--response-json", raw, "--timings"})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := runMusic(context.Background(), o, client, strings.NewReader(""), io.Discard, &log); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(song); !bytes.Equal(data, testMP3) {
		t.Fatalf("song = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "song-lyrics.txt")); string(data) != lyrics+"\n" {
		t.Fatalf("lyrics = %q", data)
	}
	if data, _ := os.ReadFile(raw); !json.Valid(data) || !bytes.Contains(data, []byte(`"model_output"`)) {
		t.Fatalf("response JSON = %.80q", data)
	}
	input := requests[0]["input"].([]any)
	if text := input[0].(map[string]any)["text"]; text != "sunny pop\n\nUse the following lyrics:\n\n[Verse 1]\nBananas at dawn" {
		t.Fatalf("prompt = %q", text)
	}
	if image := input[1].(map[string]any); image["type"] != "image" || image["mime_type"] != "image/png" {
		t.Fatalf("image = %v", image)
	}
	if _, ok := requests[0]["response_format"]; ok || requests[0]["store"] != false {
		t.Fatalf("mp3 request = %v", requests[0])
	}
	for _, want := range []string{"Generating music with lyria-3.5", "song.mp3 (MP3, 0.0 MB)", "Lyrics saved to:", "Response JSON saved to:", "Tokens: 14 input, 262 output"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log.String())
		}
	}

	// WAV, prompt from stdin, instrumental: no lyrics file for the marker.
	beat := filepath.Join(dir, "beat.wav")
	o, err = parseMusicArgs([]string{"-f", "-", "-m", "clip", "--instrumental", "-o", beat})
	if err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := runMusic(context.Background(), o, client, strings.NewReader("lo-fi beat\n"), io.Discard, &log); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(beat); !bytes.Equal(data, testWAV()) {
		t.Fatal("wav output differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "beat-lyrics.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instrumental marker should not be saved as lyrics: %v", err)
	}
	if requests[1]["model"] != "lyria-3-clip-preview" || requests[1]["input"] != "lo-fi beat\n\nInstrumental only, no vocals." {
		t.Fatalf("clip request = %v", requests[1])
	}
	if !strings.Contains(log.String(), "beat.wav (1s, WAV 48000 Hz stereo, 0.2 MB)") {
		t.Errorf("log = %s", log.String())
	}

	// Stdout carries only audio; progress goes to the log writer.
	o, err = parseMusicArgs([]string{"sunny pop", "-o", "-"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	log.Reset()
	if err := runMusic(context.Background(), o, client, strings.NewReader(""), &stdout, &log); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), testMP3) || !strings.Contains(log.String(), "Audio saved to: stdout") {
		t.Fatalf("stdout = %q, log = %s", stdout.Bytes(), log.String())
	}
}

func TestRunMusicKeepsUnexpectedAudio(t *testing.T) {
	dir := t.TempDir()
	var requests []map[string]any
	server := musicServer(t, &requests, func(map[string]any) []string {
		return []string{audioBlock("audio/mpeg", testMP3), audioBlock("audio/wav", testWAV())}
	})
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL, httpClient: server.Client()}
	o, err := parseMusicArgs([]string{"song", "-o", filepath.Join(dir, "take.wav")})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := runMusic(context.Background(), o, client, strings.NewReader(""), io.Discard, &log); err != nil {
		t.Fatal(err)
	}
	// The first track came back as MP3 despite the WAV request; the second is numbered.
	if data, _ := os.ReadFile(filepath.Join(dir, "take.mp3")); !bytes.Equal(data, testMP3) {
		t.Fatalf("take.mp3 = %q\n%s", data, log.String())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "take-2.wav")); !bytes.Equal(data, testWAV()) {
		t.Fatalf("take-2.wav missing\n%s", log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "take.wav")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("MP3 bytes must not be saved under a .wav name")
	}
	if !strings.Contains(log.String(), "Note: Gemini returned MP3 audio") {
		t.Errorf("log = %s", log.String())
	}
}

func TestGenerateMusicErrors(t *testing.T) {
	mode := ""
	responses := map[string]string{
		"status":  `{"status":"failed","error":{"message":"prompt blocked test-key"}}`,
		"text":    `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"I cannot sing that."}]}]}`,
		"empty":   `{"status":"completed","steps":[]}`,
		"base64":  `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"audio","mime_type":"audio/mpeg","data":"!!"}]}]}`,
		"unknown": `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"audio","mime_type":"audio/ogg","data":"T2dnUw=="}]}]}`,
		"json":    `not json`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "http" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported artist reference for key test-key"}}`)
			return
		}
		_, _ = io.WriteString(w, responses[mode])
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL, httpClient: server.Client()}
	request := musicRequest{Model: "lyria-3.5", Prompt: "song"}
	for name, want := range map[string]string{
		"http": "Gemini music API returned 400 Bad Request: Unsupported artist reference", "status": "prompt blocked",
		"text": "did not contain audio: I cannot sing that.", "empty": "did not contain audio",
		"base64": "decode Gemini audio data", "unknown": "unsupported audio type", "json": "decode Gemini music response",
	} {
		mode = name
		_, err := client.generateMusic(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "test-key") {
			t.Errorf("%s: API key leaked: %v", name, err)
		}
	}
	if _, err := (&geminiClient{}).generateMusic(context.Background(), request); err == nil {
		t.Error("a missing API key should fail before any request")
	}
}

func TestLoadMusicImages(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	jpeg := write("a.jpg", []byte("\xff\xd8\xff\xe0rest"))
	images, err := loadMusicImages([]string{jpeg})
	if err != nil || len(images) != 1 || images[0].MIMEType != "image/jpeg" {
		t.Fatalf("jpeg: %+v, %v", images, err)
	}
	for _, path := range []string{
		write("clip.mp4", []byte("video")),
		write("doc.pdf", []byte("%PDF-1.7")),
		write("fake.png", []byte("not a png")),
		write("empty.png", nil),
		filepath.Join(dir, "missing.png"),
	} {
		if _, err := loadMusicImages([]string{path}); err == nil {
			t.Errorf("loadMusicImages(%s) should fail", filepath.Base(path))
		}
	}
}

func TestRunMusicCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runMusicCommand(context.Background(), nil, &geminiClient{}, strings.NewReader(""), &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "generate_media music") {
		t.Fatalf("no arguments should print help: %v", err)
	}
	var usage usageError
	if err := runMusicCommand(context.Background(), []string{"song", "-o", "x.ogg"}, &geminiClient{apiKey: "k"}, strings.NewReader(""), &stdout, &stderr); !errors.As(err, &usage) {
		t.Fatalf("bad arguments should be a usage error: %v", err)
	}
	if err := runMusicCommand(context.Background(), []string{"song"}, &geminiClient{}, strings.NewReader(""), &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("missing key: %v", err)
	}
}
