package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const ttsHelp = `generate_media tts — Text to speech with Gemini 3.8 TTS.

Usage: generate_media tts [options] <text>
       generate_media tts [options] --file <path|->
       generate_media tts voices [filters]          (see: tts voices --help)
       generate_media tts voice <design|remix|clone|get|delete> ...

Options:
  -o, --output <path>      .wav (default: output.wav), raw .pcm/.raw/.l16,
                           .ulaw/.mulaw, or .alaw; "-" writes WAV to stdout
  -m, --model <name>       flash (default), flash-lite, or a full model ID
  -v, --voice <name>       Prebuilt voice, library voice ID, voice_… or voicekey_…
                           (default: Kore)
  -s, --style <text>       Delivery direction, e.g. "whispered, nervous"
  -f, --file <path>        Read the text from a file ("-" for stdin)
      --speaker <NAME=VOICE>
                           Dialogue speaker; repeat for each speaker. Text lines
                           start with "NAME:" or "NAME (style):"
      --per-turn           Synthesize dialogue one turn at a time (automatic for
                           more than two speakers or custom voices)
      --gap <duration>     Silence between per-turn dialogue lines (default 250ms)
      --language <code>    BCP-47 language hint (models auto-detect by default)
      --format <encoding>  pcm (24 kHz, default), mulaw, or alaw (8 kHz G.711)
      --seed <n>           Sampling seed for more repeatable takes
      --stream             Stream audio as it is generated (lower latency)
      --play               Play the saved WAV when done
      --response-json <path>
                           Save the raw API response (not with --stream)
      --timings            Report API time, time to first audio, and tokens
  -h, --help               Show this help

Models:
  flash       gemini-3.8-flash-tts        Expressive, 130+ languages (default)
  flash-lite  gemini-3.8-flash-lite-tts   Faster and cheaper, 100+ languages

Prebuilt voices (run 'generate_media tts voices --builtin' for details):
  Achernar Achird Algenib Algieba Alnilam Aoede Autonoe Callirrhoe Charon
  Despina Enceladus Erinome Fenrir Gacrux Iapetus Kore Laomedeia Leda Orus
  Puck Pulcherrima Rasalgethi Sadachbia Sadaltager Schedar Sulafat Umbriel
  Vindemiatrix Zephyr Zubenelgenubi

Direction (the text is read verbatim; put instructions in --style):
  Inline tags   <sigh> <laugh> <gasp> <whispering> <short pause> <long pause>
                <breath> <chuckle> <cough> <groan> <yawn> … keep tags in English
  Emphasis      Capitalize a WORD; use commas, dashes and ellipses for pacing
  Overlap       In dialogue, wrap a listener's reaction in pipes: |oh hmm|

Examples:
  generate_media tts "Have a wonderful day!" -v Puck -s "cheerful and friendly"
  generate_media tts "Wait... <short pause> did you hear that? <gasp>" -s "whispered, nervous"
  generate_media tts -f chapter.txt -m flash-lite -o chapter.wav --stream --play
  generate_media tts --speaker Joe=Puck --speaker Jane=Kore -o chat.wav \
    $'Joe: How is it going, Jane?\nJane (warm, relaxed): Not bad |mm| and you?'
  generate_media tts "Bonjour tout le monde" --language fr-FR --format mulaw -o hello.ulaw
  generate_media tts "Streaming straight to a player" --stream -o - | ffplay -nodisp -autoexit -

Multi-speaker text with two prebuilt voices uses Gemini's native dialogue.
More speakers, or custom voice_/voicekey_ voices, are synthesized per turn and
joined with --gap silence. All output carries Google's SynthID watermark.
`

type ttsOptions struct {
	text, file, output, model, voice, style, language string
	encoding, responseJSON                            string
	speakers                                          []ttsSpeaker
	seed                                              *int64
	gap                                               time.Duration
	stream, play, timings, perTurn, help, wav         bool
}

// cliArgs is a small parser shared by the tts subcommands. Flags may appear
// before or after positionals; "--" ends flag parsing.
type cliArgs struct {
	values     map[string][]string
	bools      map[string]bool
	positional []string
}

func (a cliArgs) last(name string) string {
	values := a.values[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func (a cliArgs) has(name string) bool {
	_, ok := a.values[name]
	return ok
}

// defineCLIFlags registers alias maps (e.g. "-o" and "--output" to "--output")
// as repeatable value flags and boolean flags.
func defineCLIFlags(flags *pflag.FlagSet, valueFlags, boolFlags map[string]string) {
	shorthand := map[string]string{}
	for alias, canonical := range valueFlags {
		if !strings.HasPrefix(alias, "--") {
			shorthand[canonical] = strings.TrimPrefix(alias, "-")
		}
	}
	for alias, canonical := range boolFlags {
		if !strings.HasPrefix(alias, "--") {
			shorthand[canonical] = strings.TrimPrefix(alias, "-")
		}
	}
	for _, canonical := range valueFlags {
		if name := strings.TrimPrefix(canonical, "--"); flags.Lookup(name) == nil {
			flags.StringArrayP(name, shorthand[canonical], nil, "")
		}
	}
	for _, canonical := range boolFlags {
		if name := strings.TrimPrefix(canonical, "--"); flags.Lookup(name) == nil {
			flags.BoolP(name, shorthand[canonical], false, "")
		}
	}
}

// parseCLI parses args with the flag rules shared by every generate_media
// command and reports values under canonical names (e.g. "--output").
func parseCLI(args []string, valueFlags, boolFlags map[string]string) (cliArgs, error) {
	parsed := cliArgs{values: map[string][]string{}, bools: map[string]bool{}}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	defineCLIFlags(flags, valueFlags, boolFlags)
	if err := flags.Parse(args); err != nil {
		return parsed, err
	}
	var flagErr error
	flags.Visit(func(f *pflag.Flag) {
		canonical := "--" + f.Name
		slice, ok := f.Value.(pflag.SliceValue)
		if !ok {
			parsed.bools[canonical] = f.Value.String() == "true"
			return
		}
		for _, value := range slice.GetSlice() {
			if strings.TrimSpace(value) == "" && flagErr == nil {
				flagErr = fmt.Errorf("%s requires a nonempty value", canonical)
			} else if strings.HasPrefix(value, "-") && value != "-" && flagErr == nil {
				// The next option was consumed as this flag's value.
				flagErr = fmt.Errorf("%s requires a value", canonical)
			}
		}
		parsed.values[canonical] = slice.GetSlice()
	})
	parsed.positional = flags.Args()
	return parsed, flagErr
}

var ttsValueFlags = map[string]string{
	"-o": "--output", "--output": "--output",
	"-m": "--model", "--model": "--model",
	"-v": "--voice", "--voice": "--voice",
	"-s": "--style", "--style": "--style",
	"-f": "--file", "--file": "--file",
	"--speaker": "--speaker", "--gap": "--gap", "--language": "--language",
	"--format": "--format", "--seed": "--seed", "--response-json": "--response-json",
}

var ttsBoolFlags = map[string]string{
	"-h": "--help", "--help": "--help", "--stream": "--stream", "--play": "--play",
	"--timings": "--timings", "--per-turn": "--per-turn",
}

func parseTTSArgs(args []string) (ttsOptions, error) {
	o := ttsOptions{output: "output.wav", model: defaultTTSModel, voice: defaultTTSVoice, gap: 250 * time.Millisecond}
	parsed, err := parseCLI(args, ttsValueFlags, ttsBoolFlags)
	if err != nil {
		return o, err
	}
	if parsed.bools["--help"] {
		o.help = true
		return o, nil
	}
	o.stream, o.play, o.timings, o.perTurn = parsed.bools["--stream"], parsed.bools["--play"], parsed.bools["--timings"], parsed.bools["--per-turn"]
	for flag, target := range map[string]*string{
		"--output": &o.output, "--model": &o.model, "--voice": &o.voice, "--style": &o.style,
		"--file": &o.file, "--language": &o.language, "--response-json": &o.responseJSON,
	} {
		if parsed.has(flag) {
			*target = strings.TrimSpace(parsed.last(flag))
		}
	}
	if id, ok := ttsModels[o.model]; ok {
		o.model = id
	}
	if strings.ContainsAny(o.model, "/?# \t\n") {
		return o, errors.New("invalid model ID")
	}
	if strings.ContainsAny(o.voice, " \t\n") {
		return o, errors.New("voice names and IDs cannot contain whitespace")
	}
	if o.language != "" && strings.Trim(o.language, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		return o, errors.New("language must be a BCP-47 code such as en-US")
	}

	switch {
	case o.file != "" && len(parsed.positional) > 0:
		return o, errors.New("provide text either as an argument or with --file, not both")
	case o.file == "" && (len(parsed.positional) != 1 || strings.TrimSpace(parsed.positional[0]) == ""):
		return o, errors.New("provide one quoted, nonempty text argument or --file")
	case o.file == "":
		o.text = parsed.positional[0]
	}

	if parsed.has("--gap") {
		gap, err := time.ParseDuration(parsed.last("--gap"))
		if err != nil {
			if ms, convErr := strconv.Atoi(parsed.last("--gap")); convErr == nil {
				gap, err = time.Duration(ms)*time.Millisecond, nil
			}
		}
		if err != nil || gap < 0 || gap > 10*time.Second {
			return o, errors.New("--gap must be a duration from 0 to 10s, e.g. 300ms")
		}
		o.gap = gap
	}
	if parsed.has("--seed") {
		seed, err := strconv.ParseInt(parsed.last("--seed"), 10, 64)
		if err != nil {
			return o, errors.New("--seed must be an integer")
		}
		o.seed = &seed
	}

	for _, raw := range parsed.values["--speaker"] {
		name, voice, ok := strings.Cut(raw, "=")
		name, voice = strings.TrimSpace(name), strings.TrimSpace(voice)
		if !ok || name == "" || voice == "" || strings.ContainsAny(name, ":()\n") || strings.ContainsAny(voice, " \t\n") {
			return o, fmt.Errorf("--speaker must look like NAME=VOICE: %q", raw)
		}
		for _, existing := range o.speakers {
			if strings.EqualFold(existing.Name, name) {
				return o, fmt.Errorf("duplicate speaker: %s", name)
			}
		}
		o.speakers = append(o.speakers, ttsSpeaker{Name: name, Voice: voice})
	}
	if len(o.speakers) == 1 {
		return o, errors.New("dialogue needs at least two --speaker values; use --voice for one speaker")
	}
	if len(o.speakers) > 0 && parsed.has("--voice") {
		return o, errors.New("--voice cannot be combined with --speaker")
	}
	if len(o.speakers) == 0 && (o.perTurn || parsed.has("--gap")) {
		return o, errors.New("--per-turn and --gap apply only to --speaker dialogue")
	}

	encoding, wav, err := ttsOutputEncoding(o.output)
	if err != nil {
		return o, err
	}
	if parsed.has("--format") {
		requested, err := normalizeEncoding(parsed.last("--format"))
		if err != nil {
			return o, err
		}
		if !wav && requested != encoding {
			return o, fmt.Errorf("--format %s conflicts with the %s output extension", parsed.last("--format"), filepath.Ext(o.output))
		}
		encoding = requested
	}
	o.encoding, o.wav = encoding, wav

	if o.play && (o.output == "-" || !wav) {
		return o, errors.New("--play requires a .wav output file")
	}
	if o.responseJSON != "" {
		if o.stream {
			return o, errors.New("--response-json cannot be combined with --stream")
		}
		if collision, err := pathsCollide(o.responseJSON, o.output, o.file); err != nil {
			return o, err
		} else if collision {
			return o, errors.New("response JSON path must differ from the audio output and text file")
		}
	}
	if o.file != "" && o.file != "-" && o.output != "-" {
		if collision, err := sameLocalPath(o.file, o.output); err != nil {
			return o, err
		} else if collision {
			return o, errors.New("audio output must differ from the text file")
		}
	}
	return o, nil
}

func normalizeEncoding(value string) (string, error) {
	switch strings.ToLower(value) {
	case "pcm", "l16", "linear16", "wav":
		return encodingL16, nil
	case "mulaw", "ulaw", "mu-law", "u-law":
		return encodingMulaw, nil
	case "alaw", "a-law":
		return encodingAlaw, nil
	}
	return "", fmt.Errorf("unsupported audio format %q: use pcm, mulaw, or alaw", value)
}

// ttsOutputEncoding infers the encoding and container from the output path.
func ttsOutputEncoding(path string) (encoding string, wav bool, err error) {
	if path == "-" {
		return encodingL16, true, nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav":
		return encodingL16, true, nil
	case ".pcm", ".raw", ".l16":
		return encodingL16, false, nil
	case ".ulaw", ".mulaw", ".ul", ".mu":
		return encodingMulaw, false, nil
	case ".alaw", ".al":
		return encodingAlaw, false, nil
	}
	return "", false, errors.New("audio output must end in .wav, .pcm, .raw, .l16, .ulaw, .mulaw, or .alaw (or be -)")
}

// parseDialogue splits "NAME: text" and "NAME (style): text" lines into turns.
// Unprefixed lines continue the previous turn; blank lines are ignored.
func parseDialogue(text string, speakers []ttsSpeaker) ([]ttsTurn, error) {
	var turns []ttsTurn
	for number, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		speaker, style, body, ok := splitDialogueLine(line, speakers)
		if !ok {
			if len(turns) == 0 {
				return nil, fmt.Errorf("line %d must start with a speaker name such as %q", number+1, speakers[0].Name+":")
			}
			turns[len(turns)-1].Text += "\n" + line
			continue
		}
		if body == "" {
			return nil, fmt.Errorf("line %d has no text for %s", number+1, speaker)
		}
		turns = append(turns, ttsTurn{Speaker: speaker, Style: style, Text: body})
	}
	if len(turns) == 0 {
		return nil, errors.New("dialogue has no turns")
	}
	return turns, nil
}

func splitDialogueLine(line string, speakers []ttsSpeaker) (speaker, style, body string, ok bool) {
	head, body, found := strings.Cut(line, ":")
	if !found {
		return "", "", "", false
	}
	head = strings.TrimSpace(head)
	if open := strings.Index(head, "("); open >= 0 && strings.HasSuffix(head, ")") {
		style = strings.TrimSpace(head[open+1 : len(head)-1])
		head = strings.TrimSpace(head[:open])
	}
	for _, candidate := range speakers {
		if strings.EqualFold(candidate.Name, head) {
			return candidate.Name, style, strings.TrimSpace(body), true
		}
	}
	return "", "", "", false
}

func isCustomVoice(voice string) bool {
	return strings.HasPrefix(voice, "voice_") || strings.HasPrefix(voice, "voicekey_")
}

type speechSynthesizer interface {
	synthesizeSpeech(context.Context, speechRequest, bool, func(audioFormat, []byte) error) (speechResult, error)
}

func readTTSText(o ttsOptions, stdin io.Reader) (string, error) {
	if o.file == "" {
		return o.text, nil
	}
	var data []byte
	var err error
	if o.file == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, 1<<20+1))
	} else {
		data, err = os.ReadFile(o.file)
	}
	if err != nil {
		return "", fmt.Errorf("read text: %w", err)
	}
	if len(data) > 1<<20 {
		return "", errors.New("text input exceeds 1 MiB")
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", errors.New("text input is empty")
	}
	return string(data), nil
}

// runTTS synthesizes speech. Progress goes to log so stdout can carry audio.
func runTTS(ctx context.Context, o ttsOptions, client speechSynthesizer, stdin io.Reader, stdout, log io.Writer) error {
	start := time.Now()
	text, err := readTTSText(o, stdin)
	if err != nil {
		return err
	}
	var requests []speechRequest
	base := speechRequest{Model: o.model, Language: o.language, Encoding: o.encoding, Seed: o.seed}
	mode := "single"
	if len(o.speakers) == 0 {
		request := base
		request.Voice = o.voice
		request.Turns = []ttsTurn{{Style: o.style, Text: text}}
		requests = append(requests, request)
	} else {
		turns, err := parseDialogue(text, o.speakers)
		if err != nil {
			return err
		}
		for i := range turns {
			if turns[i].Style == "" {
				turns[i].Style = o.style
			}
		}
		native := len(o.speakers) == 2 && !o.perTurn
		for _, speaker := range o.speakers {
			native = native && !isCustomVoice(speaker.Voice)
		}
		if native {
			mode = "dialogue"
			request := base
			request.Speakers = o.speakers
			request.Turns = turns
			requests = append(requests, request)
		} else {
			mode = "per-turn"
			voices := map[string]string{}
			for _, speaker := range o.speakers {
				voices[speaker.Name] = speaker.Voice
			}
			for _, turn := range turns {
				request := base
				request.Voice = voices[turn.Speaker]
				request.Turns = []ttsTurn{{Style: turn.Style, Text: turn.Text}}
				requests = append(requests, request)
			}
		}
	}

	switch mode {
	case "dialogue":
		fmt.Fprintf(log, "Generating %d-turn dialogue with %s...\n", len(requests[0].Turns), o.model)
	case "per-turn":
		fmt.Fprintf(log, "Generating %d dialogue turns one at a time with %s...\n", len(requests), o.model)
	default:
		fmt.Fprintf(log, "Generating speech with %s (voice %s)...\n", o.model, o.voice)
	}

	var destination io.Writer = stdout
	var file *os.File
	if o.output != "-" {
		file, err = os.CreateTemp(filepath.Dir(o.output), ".generate-media-tts-*")
		if err != nil {
			return fmt.Errorf("create audio output: %w", err)
		}
		defer os.Remove(file.Name())
		defer file.Close()
		if err := file.Chmod(0o644); err != nil {
			return fmt.Errorf("create audio output: %w", err)
		}
		destination = file
	}
	sink := &audioSink{w: destination, wav: o.wav}

	var apiTime, firstAudio time.Duration
	var usage ttsUsage
	var raws []json.RawMessage
	record := func(result speechResult) {
		usage.TotalInputTokens += result.Usage.TotalInputTokens
		usage.TotalOutputTokens += result.Usage.TotalOutputTokens
		if len(result.Raw) > 0 {
			raws = append(raws, result.Raw)
		}
	}
	if o.stream || len(requests) == 1 {
		// Sequential: stream each request straight into the sink.
		for i, request := range requests {
			if i > 0 {
				if err := sink.writeSilence(o.gap); err != nil {
					return err
				}
			}
			apiStart := time.Now()
			result, err := client.synthesizeSpeech(ctx, request, o.stream, func(format audioFormat, data []byte) error {
				if firstAudio == 0 {
					firstAudio = time.Since(start)
				}
				return sink.write(format, data)
			})
			apiTime += time.Since(apiStart)
			if err != nil {
				return turnError(err, i, len(requests))
			}
			record(result)
		}
	} else {
		apiStart := time.Now()
		chunks, results, err := synthesizeConcurrently(ctx, client, requests, 4)
		apiTime = time.Since(apiStart)
		if err != nil {
			return err
		}
		for i := range requests {
			if i > 0 {
				if err := sink.writeSilence(o.gap); err != nil {
					return err
				}
			}
			for _, chunk := range chunks[i] {
				if err := sink.write(chunk.format, chunk.data); err != nil {
					return err
				}
			}
			record(results[i])
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sink.finish(); err != nil {
		return err
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return fmt.Errorf("close audio output: %w", err)
		}
		if err := os.Rename(file.Name(), o.output); err != nil {
			return fmt.Errorf("save audio output: %w", err)
		}
	}
	if o.responseJSON != "" && len(raws) > 0 {
		var raw []byte = raws[0]
		if len(raws) > 1 {
			raw, err = json.Marshal(raws)
			if err != nil {
				return fmt.Errorf("encode response JSON: %w", err)
			}
		}
		if err := atomicWriteFile(o.responseJSON, raw, 0o600); err != nil {
			return fmt.Errorf("save response JSON: %w", err)
		}
	}
	length := sink.format.duration(sink.written).Round(100 * time.Millisecond)
	where := o.output
	if where == "-" {
		where = "stdout"
	}
	fmt.Fprintf(log, "Done! Audio saved to: %s (%s, %s)\n", where, length, sink.format)
	if o.responseJSON != "" {
		fmt.Fprintf(log, "Response JSON saved to: %s\n", o.responseJSON)
	}
	if o.timings {
		total := time.Since(start)
		line := fmt.Sprintf("Timing: API %s, local %s, total %s", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
		if o.stream {
			line += fmt.Sprintf(", first audio %s", firstAudio.Round(time.Millisecond))
		}
		fmt.Fprintln(log, line)
		if usage.TotalOutputTokens > 0 {
			fmt.Fprintf(log, "Tokens: %d input, %d output\n", usage.TotalInputTokens, usage.TotalOutputTokens)
		}
	}
	if o.play {
		return playAudio(ctx, o.output, log)
	}
	return nil
}

func turnError(err error, index, total int) error {
	if total <= 1 {
		return err
	}
	return fmt.Errorf("dialogue turn %d of %d: %w", index+1, total, err)
}

type audioChunk struct {
	format audioFormat
	data   []byte
}

// synthesizeConcurrently runs unary requests with bounded parallelism and
// returns each request's audio in input order. The first error cancels the rest.
func synthesizeConcurrently(ctx context.Context, client speechSynthesizer, requests []speechRequest, workers int) ([][]audioChunk, []speechResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chunks := make([][]audioChunk, len(requests))
	results := make([]speechResult, len(requests))
	errs := make([]error, len(requests))
	limit := make(chan struct{}, workers)
	var wait sync.WaitGroup
	for i := range requests {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-limit }()
			result, err := client.synthesizeSpeech(ctx, requests[i], false, func(format audioFormat, data []byte) error {
				chunks[i] = append(chunks[i], audioChunk{format: format, data: data})
				return nil
			})
			results[i], errs[i] = result, err
			if err != nil {
				cancel()
			}
		}(i)
	}
	wait.Wait()
	// Report the root cause rather than a cancellation it triggered.
	for i, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, nil, turnError(err, i, len(requests))
		}
	}
	for i, err := range errs {
		if err != nil {
			return nil, nil, turnError(err, i, len(requests))
		}
	}
	return chunks, results, nil
}

func playAudio(ctx context.Context, path string, log io.Writer) error {
	candidates := [][]string{{"ffplay", "-nodisp", "-autoexit", "-loglevel", "error"}, {"paplay"}, {"aplay", "-q"}}
	if runtime.GOOS == "darwin" {
		candidates = append([][]string{{"afplay"}}, candidates...)
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		command := exec.CommandContext(ctx, candidate[0], append(candidate[1:], path)...)
		command.Stdout, command.Stderr = log, log
		if err := command.Run(); err != nil {
			return fmt.Errorf("play audio with %s: %w", candidate[0], err)
		}
		return nil
	}
	fmt.Fprintln(log, "Warning: no audio player found (tried afplay, ffplay, paplay, aplay)")
	return nil
}

// runTTSCommand executes a tts command line, such as "voices --builtin", with this client.
func runTTSCommand(ctx context.Context, args []string, client *geminiClient, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := newTTSCmd(func() *geminiClient { return client }, stdin)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd.ExecuteContext(ctx)
}

// newTTSCmd builds the tts command tree. Each command validates its own
// arguments with parseCLI and prints its hand-written help, so cobra only
// routes to it; the flags are registered for routing and shell completion.
func newTTSCmd(client func() *geminiClient, stdin io.Reader) *cobra.Command {
	command := func(use, short, help string, valueFlags, boolFlags map[string]string, run func(*cobra.Command, []string) error) *cobra.Command {
		cmd := &cobra.Command{
			Use: use, Short: short, RunE: run, Args: cobra.ArbitraryArgs,
			DisableFlagParsing: true, SilenceUsage: true, SilenceErrors: true,
		}
		defineCLIFlags(cmd.Flags(), valueFlags, boolFlags)
		cmd.SetHelpFunc(func(cmd *cobra.Command, _ []string) { _, _ = io.WriteString(cmd.OutOrStdout(), help) })
		return cmd
	}
	tts := command("tts [options] <text>", "Generate speech from text", ttsHelp, ttsValueFlags, ttsBoolFlags,
		func(cmd *cobra.Command, args []string) error {
			return runTTSText(cmd.Context(), args, client(), stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		})
	voices := command("voices [filters]", "List prebuilt, library, and custom voices", voicesHelp, voicesValueFlags(), voicesBoolFlags,
		func(cmd *cobra.Command, args []string) error {
			return runVoicesCommand(cmd.Context(), args, client(), cmd.OutOrStdout())
		})
	voice := command("voice <design|remix|clone|get|delete>", "Design, remix, clone, inspect, or delete a custom voice", voiceHelp, voiceValueFlags, voiceBoolFlags,
		func(cmd *cobra.Command, args []string) error {
			return runVoiceCommand(cmd.Context(), args, client(), cmd.OutOrStdout())
		})
	tts.AddCommand(voices, voice)
	return tts
}

// runTTSText handles "generate_media tts [options] <text>".
func runTTSText(ctx context.Context, args []string, client *geminiClient, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := io.WriteString(stdout, ttsHelp)
		return err
	}
	o, err := parseTTSArgs(args)
	if err != nil {
		return usageError{err}
	}
	if o.help {
		_, err := io.WriteString(stdout, ttsHelp)
		return err
	}
	if err := requireAPIKey(client); err != nil {
		return err
	}
	log := stdout
	if o.output == "-" {
		log = stderr
	}
	return runTTS(ctx, o, client, stdin, stdout, log)
}

// usageError marks invalid command-line arguments. They exit with status 2
// and a pointer to the command's help; other failures exit with status 1.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func requireAPIKey(client *geminiClient) error {
	if client == nil || strings.TrimSpace(client.apiKey) == "" {
		return errors.New(missingAPIKeyMessage)
	}
	return nil
}
