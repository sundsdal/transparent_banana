package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const musicHelp = `generate_media music — Generate music with Lyria 3.5.

Usage: generate_media music [options] <prompt>
       generate_media music [options] --file <path|->

Options:
  -o, --output <path>      .mp3 (default: output.mp3) or .wav; "-" writes the
                           audio to stdout
  -m, --model <name>       song (default), clip, or a full model ID
  -i, --input <paths>      Up to 10 reference images that set the mood
                           Repeat or comma-separate paths
  -f, --file <path>        Read the prompt from a file ("-" for stdin)
      --lyrics <path>      Sing the lyrics in this file ("-" for stdin)
      --instrumental       Ask for a track without vocals
      --format <type>      mp3 or wav (default: from the output extension)
      --play               Play the saved audio when done
      --response-json <path>
                           Save the raw API response
      --timings            Report API time and tokens
  -h, --help               Show this help

Models:
  song  lyria-3.5             Full songs with verses, choruses, and bridges
                              (default)
  clip  lyria-3-clip-preview  30-second clips, loops, and previews

Prompting (Lyria takes every instruction from the prompt):
  Describe      Genre, mood, instruments, tempo (BPM), key, and length
  Structure     Tag sections in lyrics: [Verse 1] [Chorus] [Bridge]
  Timing        One segment per line: [0:00 - 0:10] Intro: soft piano
  Language      Lyrics follow the language of the prompt

Examples:
  generate_media music "An upbeat synth-pop song about a road trip at dawn"
  generate_media music "Lo-fi hip hop beat, 80 BPM, mellow Rhodes" -m clip --instrumental
  generate_media music "Dreamy indie pop, female vocals" --lyrics lyrics.txt -o dream.wav
  generate_media music -i sunset.jpg "An ambient track inspired by this image" --play
  generate_media music -f brief.txt -o - | ffplay -nodisp -autoexit -

Lyria writes lyrics unless told otherwise; they are saved beside the audio as
<output>-lyrics.txt. Each request is a single turn: to change a track, adjust
the prompt and generate again. All output carries Google's SynthID watermark.
`

type musicOptions struct {
	prompt, file, lyrics, output, model, responseJSON string
	inputs                                            []string
	instrumental, wav, play, timings, help            bool
}

var musicValueFlags = map[string]string{
	"-o": "--output", "--output": "--output",
	"-m": "--model", "--model": "--model",
	"-i": "--input", "--input": "--input",
	"-f": "--file", "--file": "--file",
	"--lyrics": "--lyrics", "--format": "--format", "--response-json": "--response-json",
}

var musicBoolFlags = map[string]string{
	"-h": "--help", "--help": "--help", "--instrumental": "--instrumental",
	"--play": "--play", "--timings": "--timings",
}

func parseMusicArgs(args []string) (musicOptions, error) {
	o := musicOptions{output: "output.mp3", model: defaultMusicModel}
	parsed, err := parseCLI(args, musicValueFlags, musicBoolFlags)
	if err != nil {
		return o, err
	}
	if parsed.bools["--help"] {
		o.help = true
		return o, nil
	}
	o.instrumental, o.play, o.timings = parsed.bools["--instrumental"], parsed.bools["--play"], parsed.bools["--timings"]
	for flag, target := range map[string]*string{
		"--output": &o.output, "--model": &o.model, "--file": &o.file,
		"--lyrics": &o.lyrics, "--response-json": &o.responseJSON,
	} {
		if parsed.has(flag) {
			*target = strings.TrimSpace(parsed.last(flag))
		}
	}
	if id, ok := musicModels[o.model]; ok {
		o.model = id
	}
	if strings.ContainsAny(o.model, "/?# \t\n") {
		return o, errors.New("invalid model ID")
	}
	for _, value := range parsed.values["--input"] {
		for _, path := range strings.Split(value, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				return o, errors.New("input paths cannot be empty")
			}
			o.inputs = append(o.inputs, path)
		}
	}
	if len(o.inputs) > maxMusicImages {
		return o, fmt.Errorf("music generation accepts up to %d reference images", maxMusicImages)
	}

	switch {
	case o.file != "" && len(parsed.positional) > 0:
		return o, errors.New("provide the prompt either as an argument or with --file, not both")
	case len(parsed.positional) > 1 || (len(parsed.positional) == 1 && strings.TrimSpace(parsed.positional[0]) == ""):
		return o, errors.New("provide one quoted, nonempty prompt")
	case len(parsed.positional) == 1:
		o.prompt = parsed.positional[0]
	case o.file == "" && o.lyrics == "":
		return o, errors.New("provide one quoted, nonempty prompt, --file, or --lyrics")
	}
	if o.file == "-" && o.lyrics == "-" {
		return o, errors.New("only one of --file and --lyrics can read stdin")
	}
	if o.instrumental && o.lyrics != "" {
		return o, errors.New("--instrumental cannot be combined with --lyrics")
	}

	format := ""
	if parsed.has("--format") {
		format = strings.ToLower(parsed.last("--format"))
		if format != "mp3" && format != "wav" {
			return o, fmt.Errorf("unsupported audio format %q: use mp3 or wav", parsed.last("--format"))
		}
		if !parsed.has("--output") {
			o.output = "output." + format
		}
	}
	if o.output == "-" {
		o.wav = format == "wav"
	} else {
		ext := strings.ToLower(filepath.Ext(o.output))
		if ext != ".mp3" && ext != ".wav" {
			return o, errors.New("audio output must end in .mp3 or .wav (or be -)")
		}
		if format != "" && "."+format != ext {
			return o, fmt.Errorf("--format %s conflicts with the %s output extension", format, ext)
		}
		o.wav = ext == ".wav"
	}
	if o.play && o.output == "-" {
		return o, errors.New("--play requires an output file")
	}

	// Nothing written may replace something read, or another output.
	var sources, written []string
	for _, path := range append([]string{o.file, o.lyrics}, o.inputs...) {
		if path != "-" {
			sources = append(sources, path)
		}
	}
	if o.output != "-" {
		written = append(written, o.output, musicLyricsPath(o.output))
	}
	written = append(written, o.responseJSON)
	for i, path := range written {
		if collision, err := pathsCollide(path, append(sources, written[:i]...)...); err != nil {
			return o, err
		} else if collision {
			return o, fmt.Errorf("output path %s must differ from the inputs and other outputs", path)
		}
	}
	return o, nil
}

// musicLyricsPath is where the lyrics Lyria returns are saved.
func musicLyricsPath(output string) string {
	return strings.TrimSuffix(output, filepath.Ext(output)) + "-lyrics.txt"
}

// composeMusicPrompt folds the lyric and vocal options into the prompt, which
// is the only control Lyria offers. Lyrics stay apart from the instructions.
func composeMusicPrompt(prompt, lyrics string, instrumental bool) string {
	prompt = strings.TrimSpace(prompt)
	sections := make([]string, 0, 2)
	if prompt != "" {
		sections = append(sections, prompt)
	}
	switch {
	case instrumental:
		sections = append(sections, "Instrumental only, no vocals.")
	case strings.TrimSpace(lyrics) != "":
		lead := "Use the following lyrics:"
		if prompt == "" {
			lead = "Create a song with the following lyrics:"
		}
		sections = append(sections, lead+"\n\n"+strings.TrimSpace(lyrics))
	}
	return strings.Join(sections, "\n\n")
}

func loadMusicImages(paths []string) ([]imageData, error) {
	images := make([]imageData, 0, len(paths))
	total := int64(0)
	for _, path := range paths {
		mimeType, _, err := mediaMIME(path)
		if err != nil || !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("music input %q must be an image (PNG, JPEG, GIF, WebP, HEIC, or HEIF)", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect input %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("input %q must be a nonempty regular file", path)
		}
		if total += info.Size(); total > maxInlineMediaBytes {
			return nil, errors.New("reference images exceed the 14 MiB request limit")
		}
		if err := validateMediaHeader(path, mimeType); err != nil {
			return nil, err
		}
		if mimeType == "image/gif" {
			// Gemini does not accept GIF; loadImage converts the first frame to PNG.
			image, err := loadImage(path)
			if err != nil {
				return nil, err
			}
			images = append(images, image)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read input %q: %w", path, err)
		}
		images = append(images, imageData{Data: data, MIMEType: mimeType})
	}
	return images, nil
}

// describeTrack summarizes saved audio. MP3 length is not reported because it
// would mean walking every frame.
func describeTrack(track musicTrack) string {
	size := fmt.Sprintf("%.1f MB", float64(len(track.Data))/1e6)
	if track.Ext == ".wav" {
		if format, samples, err := splitWAV(track.Data); err == nil {
			return fmt.Sprintf("%s, WAV %d Hz %s, %s", format.duration(int64(len(samples))).Round(100*time.Millisecond),
				format.SampleRate, map[int]string{1: "mono", 2: "stereo"}[format.Channels], size)
		}
		return "WAV, " + size
	}
	return "MP3, " + size
}

type musicGenerator interface {
	generateMusic(context.Context, musicRequest) (musicResult, error)
}

// runMusic generates one track. Progress goes to log so stdout can carry audio.
func runMusic(ctx context.Context, o musicOptions, client musicGenerator, stdin io.Reader, stdout, log io.Writer) error {
	start := time.Now()
	prompt, lyrics := o.prompt, ""
	var err error
	if o.file != "" {
		if prompt, err = readTextInput(o.file, stdin); err != nil {
			return err
		}
	}
	if o.lyrics != "" {
		if lyrics, err = readTextInput(o.lyrics, stdin); err != nil {
			return err
		}
	}
	images, err := loadMusicImages(o.inputs)
	if err != nil {
		return err
	}

	fmt.Fprintf(log, "Generating music with %s...\n", o.model)
	apiStart := time.Now()
	result, err := client.generateMusic(ctx, musicRequest{
		Model: o.model, Prompt: composeMusicPrompt(prompt, lyrics, o.instrumental), Images: images, WAV: o.wav,
	})
	apiTime := time.Since(apiStart)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(result.Tracks) == 0 {
		return errors.New("Gemini response did not contain audio")
	}

	var saved []string
	if o.output == "-" {
		if _, err := stdout.Write(result.Tracks[0].Data); err != nil {
			return fmt.Errorf("write audio: %w", err)
		}
		if extra := len(result.Tracks) - 1; extra > 0 {
			fmt.Fprintf(log, "Note: %d more returned track(s) were not written to stdout\n", extra)
		}
		saved = append(saved, "stdout")
	} else {
		for i, track := range result.Tracks {
			path := numberedImagePath(o.output, i+1)
			// Keep the audio even if Gemini ignored the requested container.
			if ext := filepath.Ext(path); !strings.EqualFold(ext, track.Ext) {
				path = strings.TrimSuffix(path, ext) + track.Ext
				fmt.Fprintf(log, "Note: Gemini returned %s audio; saving to %s\n", strings.ToUpper(track.Ext[1:]), path)
			}
			if err := atomicWriteFile(path, track.Data, 0o644); err != nil {
				return err
			}
			saved = append(saved, path)
		}
	}
	lyricsPath := ""
	if text := strings.TrimSpace(strings.Join(result.Text, "\n\n")); o.output != "-" && text != "" && text != instrumentalMarker {
		lyricsPath = musicLyricsPath(o.output)
		if err := atomicWriteFile(lyricsPath, []byte(text+"\n"), 0o644); err != nil {
			return err
		}
	}
	if o.responseJSON != "" {
		if err := atomicWriteFile(o.responseJSON, result.Raw, 0o600); err != nil {
			return fmt.Errorf("save response JSON: %w", err)
		}
	}

	for i, path := range saved {
		fmt.Fprintf(log, "Done! Audio saved to: %s (%s)\n", path, describeTrack(result.Tracks[i]))
	}
	if lyricsPath != "" {
		fmt.Fprintf(log, "Lyrics saved to: %s\n", lyricsPath)
	}
	if o.responseJSON != "" {
		fmt.Fprintf(log, "Response JSON saved to: %s\n", o.responseJSON)
	}
	if o.timings {
		total := time.Since(start)
		fmt.Fprintf(log, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
		if result.Usage.TotalOutputTokens > 0 {
			fmt.Fprintf(log, "Tokens: %d input, %d output\n", result.Usage.TotalInputTokens, result.Usage.TotalOutputTokens)
		}
	}
	if o.play {
		return playAudio(ctx, saved[0], log)
	}
	return nil
}

// newMusicCmd builds "generate_media music [options] <prompt>".
func newMusicCmd(client func() *geminiClient, stdin io.Reader) *cobra.Command {
	return newParsedCmd("music [options] <prompt>", "Generate music with Lyria", musicHelp, musicValueFlags, musicBoolFlags,
		func(cmd *cobra.Command, args []string) error {
			return runMusicCommand(cmd.Context(), args, client(), stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
		})
}

// runMusicCommand handles the arguments of "generate_media music".
func runMusicCommand(ctx context.Context, args []string, client *geminiClient, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := io.WriteString(stdout, musicHelp)
		return err
	}
	o, err := parseMusicArgs(args)
	if err != nil {
		return usageError{err}
	}
	if o.help {
		_, err := io.WriteString(stdout, musicHelp)
		return err
	}
	if err := requireAPIKey(client); err != nil {
		return err
	}
	log := stdout
	if o.output == "-" {
		log = stderr
	}
	return runMusic(ctx, o, client, stdin, stdout, log)
}
