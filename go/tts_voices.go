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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
)

const voicesHelp = `Usage: generate_media tts voices [filters] [--all] [--json]
       generate_media tts voices --builtin

Lists prebuilt voices, the Extended Voice Library (2,000+ voices), and your
custom voices. Pass a listed ID to 'generate_media tts --voice <id>'.

Filters (repeat or comma-separate for OR within a filter):
      --search <text>       Free-text search
      --language <code>     BCP-47 language, e.g. en-GB
      --region <code>       Region, e.g. US, GB
      --gender <value>      female, male, or neutral
      --pitch <value>       low, medium, or high
      --type <value>        prebuilt, prompted, or replicated
      --accent <value>      e.g. "Glasgow English"
      --persona <value>     Persona name
      --context <value>     Use-case context, e.g. "Enterprise Agent"
      --page-size <n>       Results per page (1–1000, default 50)
      --page-token <token>  Continue a previous listing with the same filters
      --all                 Follow every page
      --json                Print raw API JSON
      --builtin             Print the 30 prebuilt voices without calling the API
`

const voiceHelp = `Usage:
  generate_media tts voice design --description <text> [--name <label>]
                                  [--gender <g>] [--language <code>] [-m <model>] [-o sample.wav]
  generate_media tts voice remix  --base <voice> --description <changes> [--name <label>]
                                  [-m <model>] [-o sample.wav]
  generate_media tts voice clone  --source <speech.wav> --consent <consent.wav>
                                  [--name <label>] [--no-store] [-m <model>]
  generate_media tts voice get    <voice_id> [-o sample.wav] [--json]
  generate_media tts voice delete <voice_id>

design  Create a stored voice from a natural-language description.
remix   Create a new stored voice by describing changes to an existing one
        (prebuilt name, library ID, or voice_ ID). The base voice is unchanged.
clone   Replicate a real speaker. --source is 10–30 s of clean speech and
        --consent is the same adult speaker reading Google's consent statement,
        e.g. "I am the owner of this voice and I consent to Google using this
        voice to create a synthetic voice model." Only clone voices you have
        permission to use. --no-store returns a 7-day voicekey_ instead of a
        stored voice_ ID.

Custom voices need a paid-tier project (up to 200 stored voices). Stored voices
expire one year after last use. Design and remix save a sample WAV (default:
<voice_id>-sample.wav). Use a custom voice with 'generate_media tts --voice <id>'.
`

type voiceResource struct {
	ID           string `json:"id,omitempty"`
	Key          string `json:"key,omitempty"`
	Type         string `json:"type,omitempty"`
	Model        string `json:"model,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
	RegionCode   string `json:"region_code,omitempty"`
	Accent       string `json:"accent,omitempty"`
	Persona      string `json:"persona,omitempty"`
	Context      string `json:"context,omitempty"`
	Gender       string `json:"gender,omitempty"`
	Pitch        string `json:"pitch,omitempty"`
	Description  string `json:"description,omitempty"`
	ExpireTime   string `json:"expire_time,omitempty"`
	Prompted     *struct {
		Input     string `json:"input,omitempty"`
		BaseVoice string `json:"base_voice,omitempty"`
	} `json:"prompted,omitempty"`
	Replicated  *voiceReplicated  `json:"replicated,omitempty"`
	SampleAudio *geminiAudioBytes `json:"sample_audio,omitempty"`
}

type geminiAudioBytes struct {
	MIMEType string `json:"mime_type"`
	Data     string `json:"data"`
}

type voiceReplicated struct {
	SourceAudio  geminiAudioBytes `json:"source_audio"`
	ConsentAudio geminiAudioBytes `json:"consent_audio"`
}

type prebuiltVoice struct{ Name, Gender, Trait string }

// prebuiltVoices are the 30 voices every Gemini 3.8 TTS model accepts by name.
var prebuiltVoices = []prebuiltVoice{
	{"Achernar", "female", "Soft"}, {"Achird", "male", "Friendly"}, {"Algenib", "male", "Gravelly"},
	{"Algieba", "male", "Smooth"}, {"Alnilam", "male", "Firm"}, {"Aoede", "female", "Breezy"},
	{"Autonoe", "female", "Bright"}, {"Callirrhoe", "female", "Easy-going"}, {"Charon", "male", "Informative"},
	{"Despina", "female", "Smooth"}, {"Enceladus", "male", "Breathy"}, {"Erinome", "female", "Clear"},
	{"Fenrir", "male", "Excitable"}, {"Gacrux", "female", "Mature"}, {"Iapetus", "male", "Clear"},
	{"Kore", "female", "Firm"}, {"Laomedeia", "female", "Upbeat"}, {"Leda", "female", "Youthful"},
	{"Orus", "male", "Firm"}, {"Puck", "male", "Upbeat"}, {"Pulcherrima", "female", "Forward"},
	{"Rasalgethi", "male", "Informative"}, {"Sadachbia", "male", "Lively"}, {"Sadaltager", "male", "Knowledgeable"},
	{"Schedar", "male", "Even"}, {"Sulafat", "female", "Warm"}, {"Umbriel", "male", "Easy-going"},
	{"Vindemiatrix", "female", "Gentle"}, {"Zephyr", "female", "Bright"}, {"Zubenelgenubi", "male", "Casual"},
}

var voiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var voicesFilterFlags = map[string]string{
	"--search": "search", "--language": "language_code", "--region": "region_code", "--gender": "gender",
	"--pitch": "pitch", "--type": "type", "--accent": "accent", "--persona": "persona", "--context": "context",
}

func runVoicesCommand(ctx context.Context, args []string, client *geminiClient, out io.Writer) error {
	valueFlags := map[string]string{"--page-size": "--page-size", "--page-token": "--page-token"}
	for flag := range voicesFilterFlags {
		valueFlags[flag] = flag
	}
	parsed, err := parseCLI(args, valueFlags, map[string]string{"-h": "--help", "--help": "--help", "--all": "--all", "--json": "--json", "--builtin": "--builtin"})
	if err != nil {
		return usageError{err}
	}
	if parsed.bools["--help"] {
		_, err := io.WriteString(out, voicesHelp)
		return err
	}
	if len(parsed.positional) > 0 {
		return usageError{fmt.Errorf("unexpected argument: %s", parsed.positional[0])}
	}
	if parsed.bools["--builtin"] {
		if len(parsed.values) > 0 || parsed.bools["--all"] || parsed.bools["--json"] {
			return usageError{errors.New("--builtin cannot be combined with other options")}
		}
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "VOICE\tGENDER\tCHARACTER")
		for _, voice := range prebuiltVoices {
			fmt.Fprintf(table, "%s\t%s\t%s\n", voice.Name, voice.Gender, voice.Trait)
		}
		return table.Flush()
	}
	query := url.Values{}
	for flag, param := range voicesFilterFlags {
		for _, value := range parsed.values[flag] {
			if param == "search" {
				query.Set(param, value)
				continue
			}
			for _, item := range strings.Split(value, ",") {
				if item = strings.TrimSpace(item); item != "" {
					query.Add(param, item)
				}
			}
		}
	}
	if parsed.has("--page-size") {
		size, err := strconv.Atoi(parsed.last("--page-size"))
		if err != nil || size < 1 || size > 1000 {
			return usageError{errors.New("--page-size must be from 1 to 1000")}
		}
		query.Set("page_size", strconv.Itoa(size))
	}
	if parsed.has("--page-token") {
		query.Set("page_token", parsed.last("--page-token"))
	}
	if err := requireAPIKey(client); err != nil {
		return err
	}
	var table *tabwriter.Writer
	if !parsed.bools["--json"] {
		table = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tTYPE\tGENDER\tPITCH\tLANGUAGE\tDESCRIPTION")
	}
	count := 0
	for page := 0; ; page++ {
		raw, err := client.voicesJSON(ctx, http.MethodGet, "/voices?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		var listing struct {
			Voices        []voiceResource `json:"voices"`
			NextPageToken string          `json:"next_page_token"`
		}
		if err := json.Unmarshal(raw, &listing); err != nil {
			return fmt.Errorf("decode voices: %w", err)
		}
		if table == nil {
			if err := printBatchJSON(out, raw); err != nil {
				return err
			}
		} else {
			for _, voice := range listing.Voices {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", dash(voice.ID), dash(voice.Type), dash(voice.Gender), dash(voice.Pitch), dash(voice.LanguageCode), voiceSummary(voice))
			}
			count += len(listing.Voices)
		}
		if listing.NextPageToken == "" || !parsed.bools["--all"] || page >= 999 {
			if table != nil {
				if err := table.Flush(); err != nil {
					return err
				}
				if count == 0 {
					fmt.Fprintln(out, "No voices matched.")
				}
				if listing.NextPageToken != "" {
					fmt.Fprintf(out, "\nMore voices: add --page-token %s (or --all)\n", listing.NextPageToken)
				}
			}
			return nil
		}
		query.Set("page_token", listing.NextPageToken)
	}
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func voiceSummary(voice voiceResource) string {
	parts := []string{}
	if voice.DisplayName != "" {
		parts = append(parts, voice.DisplayName)
	}
	if voice.Description != "" {
		parts = append(parts, voice.Description)
	} else if voice.Accent != "" {
		parts = append(parts, voice.Accent)
	}
	summary := strings.Join(strings.Fields(strings.Join(parts, ": ")), " ")
	if len(summary) > 100 {
		summary = boundedRunes(summary, 99) + "…"
	}
	return dash(summary)
}

func boundedRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func runVoiceCommand(ctx context.Context, args []string, client *geminiClient, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, err := io.WriteString(out, voiceHelp)
		return err
	}
	action := args[0]
	valueFlags := map[string]string{
		"-o": "--output", "--output": "--output", "-m": "--model", "--model": "--model",
		"--description": "--description", "--name": "--name", "--gender": "--gender", "--language": "--language",
		"--base": "--base", "--source": "--source", "--consent": "--consent",
	}
	parsed, err := parseCLI(args[1:], valueFlags, map[string]string{"-h": "--help", "--help": "--help", "--json": "--json", "--no-store": "--no-store"})
	if err != nil {
		return usageError{err}
	}
	if parsed.bools["--help"] {
		_, err := io.WriteString(out, voiceHelp)
		return err
	}
	allowed := map[string][]string{
		"design": {"--output", "--model", "--description", "--name", "--gender", "--language"},
		"remix":  {"--output", "--model", "--description", "--name", "--base"},
		"clone":  {"--model", "--name", "--source", "--consent", "--language", "--gender"},
		"get":    {"--output"},
		"delete": {},
	}
	permitted, ok := allowed[action]
	if !ok {
		return usageError{fmt.Errorf("unknown voice action: %s (use design, remix, clone, get, or delete)", action)}
	}
	for flag := range parsed.values {
		if !containsValue(permitted, flag) {
			return usageError{fmt.Errorf("%s does not apply to 'voice %s'", flag, action)}
		}
	}
	if parsed.bools["--no-store"] && action != "clone" {
		return usageError{errors.New("--no-store applies only to 'voice clone'")}
	}
	if parsed.bools["--json"] && action != "get" {
		return usageError{errors.New("--json applies only to 'voice get'")}
	}
	model := defaultTTSModel
	if parsed.has("--model") {
		model = parsed.last("--model")
		if id, ok := ttsModels[model]; ok {
			model = id
		}
		if strings.ContainsAny(model, "/?# \t\n") {
			return usageError{errors.New("invalid model ID")}
		}
	}

	switch action {
	case "get", "delete":
		if len(parsed.positional) != 1 {
			return usageError{fmt.Errorf("usage: generate_media tts voice %s <voice_id>", action)}
		}
		id, err := validateVoiceID(parsed.positional[0])
		if err != nil {
			return usageError{err}
		}
		if err := requireAPIKey(client); err != nil {
			return err
		}
		if action == "delete" {
			if _, err := client.voicesJSON(ctx, http.MethodDelete, "/voices/"+url.PathEscape(id), nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "Deleted voice: %s\n", id)
			return nil
		}
		raw, err := client.voicesJSON(ctx, http.MethodGet, "/voices/"+url.PathEscape(id), nil)
		if err != nil {
			return err
		}
		if parsed.bools["--json"] {
			return printBatchJSON(out, raw)
		}
		voice, err := decodeVoice(raw)
		if err != nil {
			return err
		}
		printVoice(out, voice)
		if parsed.has("--output") {
			return saveVoiceSample(out, voice, parsed.last("--output"))
		}
		return nil
	}

	if len(parsed.positional) > 0 {
		return usageError{fmt.Errorf("unexpected argument: %s (quote descriptions and pass them with --description)", parsed.positional[0])}
	}
	resource := voiceResource{Model: model, DisplayName: parsed.last("--name"), Gender: parsed.last("--gender"), LanguageCode: parsed.last("--language")}
	if resource.Gender != "" && !containsValue([]string{"female", "male", "neutral"}, resource.Gender) {
		return usageError{errors.New("--gender must be female, male, or neutral")}
	}
	store := true
	switch action {
	case "design", "remix":
		description := parsed.last("--description")
		if description == "" {
			return usageError{fmt.Errorf("voice %s requires --description", action)}
		}
		resource.Type = "prompted"
		resource.Prompted = &struct {
			Input     string `json:"input,omitempty"`
			BaseVoice string `json:"base_voice,omitempty"`
		}{Input: description}
		if action == "remix" {
			base := parsed.last("--base")
			if base == "" {
				return usageError{errors.New("voice remix requires --base")}
			}
			resource.Prompted.BaseVoice = base
		}
	case "clone":
		source, consent := parsed.last("--source"), parsed.last("--consent")
		if source == "" || consent == "" {
			return usageError{errors.New("voice clone requires --source and --consent recordings")}
		}
		if same, err := sameLocalPath(source, consent); err != nil {
			return err
		} else if same {
			return usageError{errors.New("--source and --consent must be different recordings")}
		}
		sourceAudio, consentAudio, err := loadCloneAudio(source, consent)
		if err != nil {
			return err
		}
		resource.Type = "replicated"
		resource.Replicated = &voiceReplicated{SourceAudio: sourceAudio, ConsentAudio: consentAudio}
		store = !parsed.bools["--no-store"]
	}
	if err := requireAPIKey(client); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"store": store, "voice": resource})
	if err != nil {
		return fmt.Errorf("encode voice request: %w", err)
	}
	fmt.Fprintf(out, "Creating %s voice...\n", resource.Type)
	raw, err := client.voicesJSON(ctx, http.MethodPost, "/voices", payload)
	if err != nil {
		return err
	}
	voice, err := decodeVoice(raw)
	if err != nil {
		return err
	}
	printVoice(out, voice)
	if voice.SampleAudio != nil && voice.SampleAudio.Data != "" {
		output := parsed.last("--output")
		if output == "" {
			name := voice.ID
			if !voiceIDPattern.MatchString(name) {
				name = "voice"
			}
			output = name + "-sample.wav"
		}
		if err := saveVoiceSample(out, voice, output); err != nil {
			return err
		}
	}
	use := voice.ID
	if use == "" {
		use = voice.Key
	}
	if use != "" {
		fmt.Fprintf(out, "Use it: generate_media tts --voice %s \"Hello!\"\n", use)
	}
	return nil
}

func validateVoiceID(value string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(value), "voices/")
	if !voiceIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid voice ID: %q", value)
	}
	return id, nil
}

// decodeVoice accepts a bare Voice or one wrapped as "voice" / "replicated_voice".
func decodeVoice(raw []byte) (voiceResource, error) {
	var response struct {
		voiceResource
		Voice           *voiceResource `json:"voice"`
		ReplicatedVoice *voiceResource `json:"replicated_voice"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return voiceResource{}, fmt.Errorf("decode voice: %w", err)
	}
	for _, candidate := range []*voiceResource{&response.voiceResource, response.Voice, response.ReplicatedVoice} {
		if candidate != nil && (candidate.ID != "" || candidate.Key != "") {
			return *candidate, nil
		}
	}
	return voiceResource{}, errors.New("Gemini voice response did not contain a voice ID or key")
}

func printVoice(out io.Writer, voice voiceResource) {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, row := range [][2]string{
		{"ID", voice.ID}, {"Key", voice.Key}, {"Type", voice.Type}, {"Name", voice.DisplayName},
		{"Model", voice.Model}, {"Gender", voice.Gender}, {"Pitch", voice.Pitch}, {"Language", voice.LanguageCode},
		{"Accent", voice.Accent}, {"Description", voice.Description}, {"Expires", voice.ExpireTime},
	} {
		if row[1] != "" {
			fmt.Fprintf(table, "%s:\t%s\n", row[0], row[1])
		}
	}
	if voice.Prompted != nil {
		if voice.Prompted.BaseVoice != "" {
			fmt.Fprintf(table, "Base voice:\t%s\n", voice.Prompted.BaseVoice)
		}
		if voice.Prompted.Input != "" {
			fmt.Fprintf(table, "Prompt:\t%s\n", voice.Prompted.Input)
		}
	}
	_ = table.Flush()
}

func saveVoiceSample(out io.Writer, voice voiceResource, path string) error {
	if voice.SampleAudio == nil || voice.SampleAudio.Data == "" {
		return errors.New("Gemini did not return sample audio for this voice")
	}
	data, err := base64.StdEncoding.DecodeString(voice.SampleAudio.Data)
	if err != nil || len(data) == 0 {
		return errors.New("Gemini returned invalid sample audio")
	}
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("save sample audio: %w", err)
	}
	fmt.Fprintf(out, "Sample saved to: %s\n", path)
	return nil
}

var cloneAudioMIME = map[string]string{
	".wav": "audio/wav", ".mp3": "audio/mpeg", ".flac": "audio/flac", ".ogg": "audio/ogg",
	".opus": "audio/ogg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".webm": "audio/webm",
}

func loadCloneAudio(source, consent string) (geminiAudioBytes, geminiAudioBytes, error) {
	var loaded [2]geminiAudioBytes
	total := int64(0)
	for i, path := range []string{source, consent} {
		mimeType, ok := cloneAudioMIME[strings.ToLower(filepath.Ext(path))]
		if !ok {
			return loaded[0], loaded[1], fmt.Errorf("recording %q must be WAV (recommended), MP3, FLAC, OGG, M4A, AAC, or WebM", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return loaded[0], loaded[1], fmt.Errorf("inspect recording %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return loaded[0], loaded[1], fmt.Errorf("recording %q must be a nonempty regular file", path)
		}
		total += info.Size()
		if total > maxInlineMediaBytes {
			return loaded[0], loaded[1], errors.New("recordings exceed the 14 MiB inline request limit; trim them to 10–30 seconds")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return loaded[0], loaded[1], fmt.Errorf("read recording %q: %w", path, err)
		}
		loaded[i] = geminiAudioBytes{MIMEType: mimeType, Data: base64.StdEncoding.EncodeToString(data)}
	}
	return loaded[0], loaded[1], nil
}

// voicesJSON calls the Voices API and returns the raw JSON body.
func (c *geminiClient) voicesJSON(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	response, err := c.apiRequest(ctx, method, path, body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := c.apiStatusError(response, "Gemini Voices API"); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read Gemini voices response: %w", err)
	}
	return raw, nil
}
