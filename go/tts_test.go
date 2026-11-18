package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTTSArgs(t *testing.T) {
	o, err := parseTTSArgs([]string{"hello", "-v", "Puck", "--style=warm", "-m", "flash-lite", "--seed", "7"})
	if err != nil || o.text != "hello" || o.voice != "Puck" || o.style != "warm" || o.model != "gemini-3.8-flash-lite-tts" || o.seed == nil || *o.seed != 7 {
		t.Fatalf("parse: %+v, %v", o, err)
	}
	if o.output != "output.wav" || o.encoding != encodingL16 || !o.wav {
		t.Fatalf("defaults: %+v", o)
	}
	o, err = parseTTSArgs([]string{"hi", "-o", "x.wav", "--format", "mulaw"})
	if err != nil || o.encoding != encodingMulaw || !o.wav {
		t.Fatalf("mulaw wav: %+v %v", o, err)
	}
	o, err = parseTTSArgs([]string{"hi", "-o", "x.alaw"})
	if err != nil || o.encoding != encodingAlaw || o.wav {
		t.Fatalf("alaw raw: %+v %v", o, err)
	}
	o, err = parseTTSArgs([]string{"--speaker", "Joe=Puck", "--speaker=Jane=Kore", "-f", "-", "--gap", "400"})
	if err != nil || len(o.speakers) != 2 || o.speakers[1] != (ttsSpeaker{"Jane", "Kore"}) || o.file != "-" || o.gap != 400*time.Millisecond {
		t.Fatalf("speakers: %+v %v", o, err)
	}
	if o, err := parseTTSArgs([]string{"--help"}); err != nil || !o.help {
		t.Fatal("help should not need text")
	}
	for _, args := range [][]string{
		nil,
		{"a", "b"},
		{"hi", "-o", "x.mp3"},
		{"hi", "-o", "x.pcm", "--format", "mulaw"},
		{"hi", "--format", "opus"},
		{"hi", "--speaker", "Joe=Puck"},
		{"hi", "--speaker", "Joe=Puck", "--speaker", "joe=Kore"},
		{"hi", "--speaker", "Joe=Puck", "--speaker", "Jane=Kore", "-v", "Kore"},
		{"hi", "--per-turn"},
		{"hi", "--play", "-o", "x.pcm"},
		{"hi", "--play", "-o", "-"},
		{"hi", "--stream", "--response-json", "r.json"},
		{"hi", "--response-json", "output.wav"},
		{"hi", "-f", "t.txt"},
		{"-f", "t.txt", "-o", "t.txt"},
		{"hi", "--seed", "x"},
		{"hi", "--gap", "1h"},
		{"hi", "-m", "../x"},
		{"hi", "--language", "en US"},
		{"hi", "--sample-rate", "16000"},
	} {
		if _, err := parseTTSArgs(args); err == nil {
			t.Errorf("accepted invalid args: %q", args)
		}
	}
}

func TestParseDialogue(t *testing.T) {
	speakers := []ttsSpeaker{{"Joe", "Puck"}, {"Dr. Anya", "Kore"}}
	turns, err := parseDialogue("Joe: Hi there.\r\n\ncontinued line\ndr. anya (warm, slow): Hello |mm| Joe: friend.\n", speakers)
	want := []ttsTurn{
		{Speaker: "Joe", Text: "Hi there.\ncontinued line"},
		{Speaker: "Dr. Anya", Style: "warm, slow", Text: "Hello |mm| Joe: friend."},
	}
	if err != nil || !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns = %#v, %v", turns, err)
	}
	for _, text := range []string{"Hello there", "Bob: hi", "Joe:   ", "\n\n"} {
		if _, err := parseDialogue(text, speakers); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestSpeechPayload(t *testing.T) {
	seed := int64(3)
	payload, err := speechRequest{Model: "m", Voice: "Kore", Language: "nb-NO", Seed: &seed, Turns: []ttsTurn{{Style: "calm", Text: "Hei"}}}.payload(false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"Hei","annotations":[{"type":"speech_metadata","style":"calm"}]}]}],"response_format":{"type":"audio","mime_type":"audio/l16"},"generation_config":{"speech_config":[{"voice":"Kore","language":"nb-NO"}],"seed":3},"stream":false,"store":false}`
	if string(payload) != want {
		t.Fatalf("payload:\n%s\nwant:\n%s", payload, want)
	}
	payload, err = speechRequest{Model: "m", Encoding: encodingMulaw, Speakers: []ttsSpeaker{{"A", "Puck"}, {"B", "Kore"}}, Turns: []ttsTurn{{Speaker: "A", Text: "x"}, {Speaker: "B", Text: "y"}}}.payload(true)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"x","annotations":[{"type":"speech_metadata","speaker":"A"}]},{"type":"text","text":"y","annotations":[{"type":"speech_metadata","speaker":"B"}]}]}],"response_format":{"type":"audio","mime_type":"audio/mulaw"},"generation_config":{"speech_config":{"speakers":[{"speaker":"A","voice":"Puck"},{"speaker":"B","voice":"Kore"}]}},"stream":true,"store":false}`
	if string(payload) != want {
		t.Fatalf("payload:\n%s\nwant:\n%s", payload, want)
	}
	for _, bad := range []speechRequest{
		{Model: "m", Voice: "Kore"},
		{Model: "m", Turns: []ttsTurn{{Text: "x"}}},
		{Model: "m", Voice: "Kore", Speakers: []ttsSpeaker{{"A", "B"}, {"C", "D"}}, Turns: []ttsTurn{{Text: "x"}}},
		{Model: "m", Speakers: []ttsSpeaker{{"A", "B"}}, Turns: []ttsTurn{{Text: "x"}}},
	} {
		if _, err := bad.payload(false); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestWAVRoundTrip(t *testing.T) {
	for _, format := range []audioFormat{{encodingL16, 24000, 1}, {encodingMulaw, 8000, 1}, {encodingAlaw, 8000, 2}} {
		samples := []byte{1, 2, 3, 4, 5, 6}
		file := append(wavHeader(format, int64(len(samples))), samples...)
		got, data, err := splitWAV(file)
		if err != nil || got != format || !bytes.Equal(data, samples) {
			t.Fatalf("%v: got %v %v %v", format, got, data, err)
		}
		if size := binary.LittleEndian.Uint32(file[4:8]); int(size) != len(file)-8 {
			t.Fatalf("%v: RIFF size %d, file %d", format, size, len(file))
		}
	}
	if _, _, err := splitWAV([]byte("RIFF....WAVEjunk")); err == nil {
		t.Fatal("accepted WAV without data")
	}
	if got := (audioFormat{encodingL16, 24000, 1}).duration(48000); got != time.Second {
		t.Fatalf("duration = %s", got)
	}
	if got := (audioFormat{encodingMulaw, 8000, 1}).silence(time.Millisecond); !bytes.Equal(got, bytes.Repeat([]byte{0xFF}, 8)) {
		t.Fatalf("mulaw silence = %v", got)
	}
}

func TestParseAudioMIME(t *testing.T) {
	format, wav, err := parseAudioMIME("audio/l16; rate=24000; channels=1")
	if err != nil || wav || format != (audioFormat{encodingL16, 24000, 1}) {
		t.Fatalf("l16: %v %v %v", format, wav, err)
	}
	format, _, err = parseAudioMIME("audio/mulaw")
	if err != nil || format != (audioFormat{encodingMulaw, 8000, 1}) {
		t.Fatalf("mulaw: %v %v", format, err)
	}
	if _, wav, err := parseAudioMIME("audio/wav"); err != nil || !wav {
		t.Fatal("wav not detected")
	}
	if _, _, err := parseAudioMIME("audio/mpeg"); err == nil {
		t.Fatal("accepted mp3")
	}
}

func pcmPart(samples []byte) string {
	return fmt.Sprintf(`{"type":"audio","mime_type":"audio/l16; rate=24000; channels=1","sample_rate":24000,"channels":1,"data":%q}`, base64.StdEncoding.EncodeToString(samples))
}

// ttsServer answers unary and streaming interactions with the request's
// first text bytes as audio, so tests can check ordering.
func ttsServer(t *testing.T, requests *[]map[string]any) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" || r.URL.Path != "/v1beta/interactions" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		*requests = append(*requests, body)
		mu.Unlock()
		content := body["input"].([]any)[0].(map[string]any)["content"].([]any)
		text := content[0].(map[string]any)["text"].(string)
		audio := []byte(text)
		if len(audio)%2 == 1 {
			audio = append(audio, 0)
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			half := len(audio) / 2
			half -= half % 2
			fmt.Fprintf(w, "event: interaction.created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{}}\n\n")
			for _, chunk := range [][]byte{audio[:half], audio[half:]} {
				fmt.Fprintf(w, "event: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"audio\",\"mime_type\":\"audio/l16\",\"sample_rate\":24000,\"channels\":1,\"data\":%q},\"event_type\":\"step.delta\"}\n\n", base64.StdEncoding.EncodeToString(chunk))
			}
			fmt.Fprintf(w, "event: interaction.completed\ndata: {\"interaction\":{\"status\":\"completed\",\"usage\":{\"total_input_tokens\":2,\"total_output_tokens\":5}},\"event_type\":\"interaction.completed\"}\n\nevent: done\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, `{"status":"completed","usage":{"total_input_tokens":2,"total_output_tokens":5},"steps":[{"type":"model_output","content":[%s]}]}`, pcmPart(audio))
	}))
}

func TestRunTTSSingleSpeakerWAV(t *testing.T) {
	var requests []map[string]any
	server := ttsServer(t, &requests)
	defer server.Close()
	dir := t.TempDir()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	for _, stream := range []bool{false, true} {
		requests = nil
		o, err := parseTTSArgs([]string{"abcd", "-o", filepath.Join(dir, "out.wav"), "-s", "warm", "--timings", "--response-json", filepath.Join(dir, "r.json")})
		if stream {
			o, err = parseTTSArgs([]string{"abcd", "-o", filepath.Join(dir, "out.wav"), "-s", "warm", "--timings", "--stream"})
		}
		if err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		if err := runTTS(context.Background(), o, client, nil, io.Discard, &log); err != nil {
			t.Fatal(err)
		}
		file, err := os.ReadFile(filepath.Join(dir, "out.wav"))
		if err != nil {
			t.Fatal(err)
		}
		format, data, err := splitWAV(file)
		if err != nil || string(data) != "abcd" || format != (audioFormat{encodingL16, 24000, 1}) {
			t.Fatalf("stream=%v: %v %q %v", stream, format, data, err)
		}
		if binary.LittleEndian.Uint32(file[40:44]) != 4 {
			t.Fatalf("stream=%v: WAV data size not finalized", stream)
		}
		if len(requests) != 1 || requests[0]["stream"] != stream {
			t.Fatalf("requests = %#v", requests)
		}
		if !strings.Contains(log.String(), "Tokens: 2 input, 5 output") {
			t.Fatalf("log = %s", log.String())
		}
		if stream && !strings.Contains(log.String(), "first audio") {
			t.Fatalf("log lacks TTFA: %s", log.String())
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "r.json"))
	if err != nil || !json.Valid(raw) {
		t.Fatalf("response json: %s %v", raw, err)
	}
}

func TestRunTTSDialogueModes(t *testing.T) {
	var requests []map[string]any
	server := ttsServer(t, &requests)
	defer server.Close()
	dir := t.TempDir()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	text := "Joe: aa\nJane (excited): bb\nJoe: cc"

	// Two prebuilt voices: one native request.
	o, err := parseTTSArgs([]string{text, "--speaker", "Joe=Puck", "--speaker", "Jane=Kore", "-o", filepath.Join(dir, "native.pcm")})
	if err != nil {
		t.Fatal(err)
	}
	if err := runTTS(context.Background(), o, client, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("native requests = %d", len(requests))
	}
	speech := requests[0]["generation_config"].(map[string]any)["speech_config"].(map[string]any)
	if len(speech["speakers"].([]any)) != 2 {
		t.Fatalf("speech config = %#v", speech)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "native.pcm")); string(data) != "aa" {
		t.Fatalf("native audio = %q", data)
	}

	// A custom voice forces per-turn synthesis, joined in order with silence.
	for _, stream := range []bool{false, true} {
		requests = nil
		args := []string{text, "--speaker", "Joe=voice_abc", "--speaker", "Jane=Kore", "-o", filepath.Join(dir, "turns.pcm"), "--gap", "1ms"}
		if stream {
			args = append(args, "--stream")
		}
		o, err = parseTTSArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := runTTS(context.Background(), o, client, nil, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if len(requests) != 3 {
			t.Fatalf("per-turn requests = %d", len(requests))
		}
		gap := strings.Repeat("\x00", 48)
		want := "aa" + gap + "bb" + gap + "cc"
		if data, _ := os.ReadFile(filepath.Join(dir, "turns.pcm")); string(data) != want {
			t.Fatalf("stream=%v per-turn audio = %q", stream, data)
		}
		voices := map[string]bool{}
		for _, request := range requests {
			config := request["generation_config"].(map[string]any)["speech_config"].([]any)[0].(map[string]any)
			voices[config["voice"].(string)] = true
			part := request["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			if part["text"] == "bb" && part["annotations"].([]any)[0].(map[string]any)["style"] != "excited" {
				t.Fatalf("turn style lost: %#v", part)
			}
		}
		if !voices["voice_abc"] || !voices["Kore"] {
			t.Fatalf("voices = %v", voices)
		}
	}
}

func TestRunTTSStdoutAndFileInput(t *testing.T) {
	var requests []map[string]any
	server := ttsServer(t, &requests)
	defer server.Close()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL + "/v1beta", httpClient: server.Client()}
	o, err := parseTTSArgs([]string{"-f", "-", "-o", "-", "--stream"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, log bytes.Buffer
	if err := runTTS(context.Background(), o, client, strings.NewReader("wxyz"), &stdout, &log); err != nil {
		t.Fatal(err)
	}
	_, data, err := splitWAV(stdout.Bytes())
	if err != nil || string(data) != "wxyz" {
		t.Fatalf("stdout WAV = %q %v", data, err)
	}
	if !strings.Contains(log.String(), "stdout") {
		t.Fatalf("log = %s", log.String())
	}
}

func TestSpeechErrors(t *testing.T) {
	responses := map[string]string{
		"status": `{"status":"failed","error":{"message":"bad voice"}}`,
		"text":   `{"status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"cannot"}]}]}`,
		"stream": "event: error\ndata: {\"error\":{\"message\":\"overloaded\",\"code\":\"unavailable\"},\"event_type\":\"error\"}\n\n",
		"short":  "event: step.delta\ndata: {\"delta\":{\"type\":\"audio\",\"mime_type\":\"audio/l16\",\"data\":\"AAA=\"},\"event_type\":\"step.delta\"}\n\n",
	}
	var mode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "http" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"No matching speaker voice found for name: Nope test-key","code":"invalid_request"}}`)
			return
		}
		_, _ = io.WriteString(w, responses[mode])
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL, httpClient: server.Client()}
	request := speechRequest{Model: "m", Voice: "Kore", Turns: []ttsTurn{{Text: "hi"}}}
	ignore := func(audioFormat, []byte) error { return nil }
	for name, want := range map[string]string{"http": "No matching speaker voice", "status": "bad voice", "text": "cannot", "stream": "overloaded", "short": "ended before completion"} {
		mode = name
		_, err := client.synthesizeSpeech(context.Background(), request, name == "stream" || name == "short", ignore)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "test-key") {
			t.Errorf("%s: API key leaked: %v", name, err)
		}
	}
}

func TestVoicesListAndDesign(t *testing.T) {
	var created map[string]any
	sample := base64.StdEncoding.EncodeToString(append(wavHeader(audioFormat{encodingL16, 24000, 1}, 2), 0, 0))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/voices":
			q := r.URL.Query()
			if !reflect.DeepEqual(q["language_code"], []string{"en-GB", "en-US"}) || q.Get("gender") != "female" || q.Get("search") != "calm, warm" {
				t.Errorf("query = %v", q)
			}
			if q.Get("page_token") == "" {
				_, _ = io.WriteString(w, `{"voices":[{"id":"en-gb-advisor-1","type":"prebuilt","gender":"female","pitch":"medium","language_code":"en-GB","display_name":"Advisor 1","description":"Calm lawyer."}],"next_page_token":"p2"}`)
				return
			}
			_, _ = io.WriteString(w, `{"voices":[{"id":"Kore","type":"prebuilt"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/voices":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			if created["voice"].(map[string]any)["type"] == "replicated" {
				_, _ = io.WriteString(w, `{"key":"voicekey_9","type":"replicated","expire_time":"2026-11-25T00:00:00Z"}`)
				return
			}
			fmt.Fprintf(w, `{"id":"voice_123","type":"prompted","display_name":"Narrator","sample_audio":{"mime_type":"audio/wav","data":%q}}`, sample)
		case r.Method == http.MethodDelete && r.URL.Path == "/voices/voice_123":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &geminiClient{apiKey: "test-key", baseURL: server.URL, httpClient: server.Client()}
	var out bytes.Buffer
	if err := runTTSCommand(context.Background(), []string{"voices", "--language", "en-GB,en-US", "--gender", "female", "--search", "calm, warm", "--all"}, client, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "en-gb-advisor-1") || !strings.Contains(out.String(), "Advisor 1: Calm lawyer.") || !strings.Contains(out.String(), "Kore") {
		t.Fatalf("listing = %s", out.String())
	}

	dir := t.TempDir()
	out.Reset()
	samplePath := filepath.Join(dir, "narrator.wav")
	if err := runTTSCommand(context.Background(), []string{"voice", "design", "--description", "A calm narrator", "--name", "Narrator", "--gender", "male", "-o", samplePath}, client, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	voice := created["voice"].(map[string]any)
	if created["store"] != true || voice["type"] != "prompted" || voice["model"] != defaultTTSModel || voice["prompted"].(map[string]any)["input"] != "A calm narrator" || voice["gender"] != "male" {
		t.Fatalf("created = %#v", created)
	}
	if data, err := os.ReadFile(samplePath); err != nil || !bytes.HasPrefix(data, []byte("RIFF")) {
		t.Fatalf("sample = %v", err)
	}
	if !strings.Contains(out.String(), "--voice voice_123") {
		t.Fatalf("design output = %s", out.String())
	}
	if err := runTTSCommand(context.Background(), []string{"voice", "delete", "voices/voice_123"}, client, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}

	source, consent := filepath.Join(dir, "source.wav"), filepath.Join(dir, "consent.wav")
	for _, path := range []string{source, consent} {
		if err := os.WriteFile(path, []byte("RIFFdata"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runTTSCommand(context.Background(), []string{"voice", "clone", "--source", source, "--consent", consent, "--no-store", "-m", "flash-lite"}, client, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	voice = created["voice"].(map[string]any)
	replicated := voice["replicated"].(map[string]any)
	if created["store"] != false || voice["type"] != "replicated" || voice["model"] != "gemini-3.8-flash-lite-tts" || replicated["source_audio"].(map[string]any)["mime_type"] != "audio/wav" {
		t.Fatalf("clone request = %#v", created)
	}

	for _, args := range [][]string{
		{"voice", "design"},
		{"voice", "remix", "--description", "older"},
		{"voice", "clone", "--source", source, "--consent", source},
		{"voice", "get"},
		{"voice", "get", "../etc"},
		{"voice", "design", "--description", "x", "--base", "Puck"},
		{"voice", "frobnicate"},
		{"voices", "--page-size", "0"},
		{"voices", "--builtin", "--json"},
	} {
		if err := runTTSCommand(context.Background(), args, client, nil, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestDecodeVoiceWrapped(t *testing.T) {
	voice, err := decodeVoice([]byte(`{"replicated_voice":{"key":"voicekey_1","expire_time":"2026-11-25T00:00:00Z"}}`))
	if err != nil || voice.Key != "voicekey_1" {
		t.Fatalf("voice = %+v %v", voice, err)
	}
	if _, err := decodeVoice([]byte(`{}`)); err == nil {
		t.Fatal("accepted empty voice")
	}
}

func TestBuiltinVoices(t *testing.T) {
	var out bytes.Buffer
	if err := runTTSCommand(context.Background(), []string{"voices", "--builtin"}, nil, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 31 || !strings.Contains(out.String(), "Zubenelgenubi") {
		t.Fatalf("builtin = %s", out.String())
	}
	if err := runTTSCommand(context.Background(), []string{"hello"}, &geminiClient{}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("missing key err = %v", err)
	}
}
