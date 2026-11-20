package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// runCLI executes the full command tree with fake generation and batch dependencies.
func runCLI(args ...string) (stdout string, generated []options, err error) {
	root := newRootCmd(
		func(_ context.Context, o options, _ io.Writer) error {
			generated = append(generated, o)
			return nil
		},
		func() (*geminiClient, error) { return nil, errors.New(missingAPIKeyMessage) },
		func() *geminiClient { return &geminiClient{} },
	)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return out.String(), generated, err
}

func isUsageError(err error) bool {
	var usage usageError
	return errors.As(err, &usage)
}

func TestCLIGeneratesWithFlagsAroundPrompt(t *testing.T) {
	for _, args := range [][]string{
		{"-m", "flash-lite", "a cat", "-a", "9:16", "-i", "a.png", "--input=b.png,c.png"},
		{"a cat", "--model=flash-lite", "-a9:16", "-ia.png", "-i", "b.png,c.png"},
	} {
		_, generated, err := runCLI(args...)
		if err != nil || len(generated) != 1 {
			t.Fatalf("%v: generated %d, err %v", args, len(generated), err)
		}
		o := generated[0]
		if o.prompt != "a cat" || o.model != models["flash-lite"] || o.aspect != "9:16" || o.input != "a.png,b.png,c.png" || o.output != "output.png" {
			t.Fatalf("%v: options %+v", args, o)
		}
	}
}

func TestCLIHelpAndVersionDoNotGenerate(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h", "extra", "words"}, {"help"}} {
		out, generated, err := runCLI(args...)
		if err != nil || len(generated) != 0 {
			t.Fatalf("%v: generated %d, err %v", args, len(generated), err)
		}
		for _, want := range []string{"General:", "Image:", "Video:", "Video jobs:", "Google Cloud video:", "--cancel-interaction-id <id>", "batch ", "tts ", "music ", "Models:"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: help is missing %q", args, want)
			}
		}
	}
	out, generated, err := runCLI()
	if err != nil || len(generated) != 0 || out != shortHelp {
		t.Fatalf("no arguments: %q, generated %d, err %v", out, len(generated), err)
	}
	out, generated, err = runCLI("--version")
	if err != nil || len(generated) != 0 || out != "generate_media version "+version+"\n" {
		t.Fatalf("version: %q, generated %d, err %v", out, len(generated), err)
	}
}

func TestCLIHelpListsEveryFlag(t *testing.T) {
	out, _, err := runCLI("--help")
	if err != nil {
		t.Fatal(err)
	}
	values := &flagValues{}
	for _, group := range values.flagGroups() {
		group.flags.VisitAll(func(f *pflag.Flag) {
			if !strings.Contains(out, "--"+f.Name) {
				t.Errorf("help is missing --%s", f.Name)
			}
			if f.Value.Type() != "bool" && len(f.Annotations[placeholderAnnotation]) == 0 {
				t.Errorf("--%s has no value placeholder", f.Name)
			}
		})
	}
}

func TestCLICommandNamesCanBePrompts(t *testing.T) {
	for _, prompt := range []string{"help", "batch", "tts", "music", "-a prompt"} {
		_, generated, err := runCLI("--", prompt)
		if err != nil || len(generated) != 1 || generated[0].prompt != prompt {
			t.Fatalf("prompt %q: %+v, %v", prompt, generated, err)
		}
	}
}

func TestCLIUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"a", "cat"},
		{""},
		{"--video"},
		{"cat", "--vidoe"},
		{"cat", "-m"},
		{"cat", "-o", "--video"},
		{"cat", "--output="},
		{"cat", "--video", "--seed", "x"},
		{"cat", "--video", "--label", "nope"},
		{"cat", "-o", "cat.webp"},
		{"--video", "batch", "status", "batches/abc"},
		{"batch", "sumbit", "requests.jsonl"},
		{"batch", "status"},
		{"batch", "status", "abc"},
		{"batch", "status", "batches/abc", "extra"},
		{"batch", "cancel"},
		{"batch", "submit"},
		{"batch", "submit", "requests.jsonl", "--wat"},
		{"batch", "download"},
		{"batch", "download", "batches/abc", "-o", "out"},
		{"batch", "download", "batches/abc", "--output-dir="},
	} {
		_, generated, err := runCLI(args...)
		if !isUsageError(err) || len(generated) != 0 {
			t.Errorf("%v: want usage error, got %v (generated %d)", args, err, len(generated))
		}
	}
	if _, _, err := runCLI("batch", "sumbit"); err == nil || !strings.Contains(err.Error(), "did you mean submit?") {
		t.Errorf("batch typo suggestion: %v", err)
	}
}

func TestCLIBatchNeedsKeyOnlyForValidCommands(t *testing.T) {
	for _, args := range [][]string{
		{"batch", "status", "batches/abc"},
		{"batch", "cancel", "batches/abc"},
		{"batch", "download", "batches/abc", "--output-dir", "out"},
		{"batch", "submit", "requests.jsonl", "--name", "my batch", "-m", "flash"},
	} {
		_, _, err := runCLI(args...)
		if err == nil || isUsageError(err) || err.Error() != missingAPIKeyMessage {
			t.Errorf("%v: want missing key error, got %v", args, err)
		}
	}
	for _, args := range [][]string{{"batch"}, {"batch", "--help"}, {"batch", "help"}, {"help", "batch"}, {"batch", "submit", "--help"}} {
		out, _, err := runCLI(args...)
		if err != nil || !strings.Contains(out, "generate_media batch") {
			t.Errorf("%v: help %q, err %v", args, out, err)
		}
	}
}

func TestCLIRuntimeErrorsAreNotUsageErrors(t *testing.T) {
	failure := errors.New("API unavailable")
	root := newRootCmd(
		func(context.Context, options, io.Writer) error { return failure },
		func() (*geminiClient, error) { return nil, failure },
		func() *geminiClient { return &geminiClient{} },
	)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"a cat"})
	if err := root.ExecuteContext(context.Background()); !errors.Is(err, failure) || isUsageError(err) {
		t.Fatalf("runtime error = %v", err)
	}
}

func TestCLITTSCommands(t *testing.T) {
	for args, want := range map[string]string{
		"tts":                  ttsHelp,
		"tts --help":           ttsHelp,
		"help tts":             ttsHelp,
		"tts voices --help":    voicesHelp,
		"help tts voices":      voicesHelp,
		"tts voice":            voiceHelp,
		"tts voice get --help": voiceHelp,
	} {
		out, _, err := runCLI(strings.Fields(args)...)
		if err != nil || out != want {
			t.Errorf("%q: unexpected help %q, err %v", args, out, err)
		}
	}
	if out, _, err := runCLI("tts", "voices", "--builtin"); err != nil || !strings.Contains(out, "Kore") {
		t.Errorf("builtin voices need no API key: %q, %v", out, err)
	}
	for _, args := range [][]string{
		{"tts", "hello", "--wat"},
		{"tts", "hello", "world"},
		{"tts", "hello", "-o", "--stream"},
		{"tts", "hello", "--gap", "300ms"},
		{"tts", "voices", "extra"},
		{"tts", "voices", "--page-size", "0"},
		{"tts", "voice", "frobnicate"},
		{"tts", "voice", "get"},
		{"tts", "voice", "delete", "voices/x", "--json"},
		{"--video", "tts", "hello"},
	} {
		if _, generated, err := runCLI(args...); !isUsageError(err) || len(generated) != 0 {
			t.Errorf("%v: want usage error, got %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"tts", "hello", "-v", "Puck", "--style=warm", "-o", "-"},
		{"tts", "-vPuck", "--", "voices"},
		{"tts", "voice", "get", "voice_123"},
	} {
		if _, _, err := runCLI(args...); err == nil || isUsageError(err) || err.Error() != missingAPIKeyMessage {
			t.Errorf("%v: want missing key error, got %v", args, err)
		}
	}
}

func TestCLIMusicCommand(t *testing.T) {
	for _, args := range []string{"music", "music --help", "help music", "music -h a song"} {
		out, _, err := runCLI(strings.Fields(args)...)
		if err != nil || out != musicHelp {
			t.Errorf("%q: unexpected help %q, err %v", args, out, err)
		}
	}
	for _, args := range [][]string{
		{"music", "a song", "--wat"},
		{"music", "a", "song"},
		{"music", "a song", "-o", "--play"},
		{"music", "a song", "-o", "song.ogg"},
		{"music", "a song", "--format="},
		{"--video", "music", "a song"},
	} {
		if _, generated, err := runCLI(args...); !isUsageError(err) || len(generated) != 0 {
			t.Errorf("%v: want usage error, got %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"music", "a song", "-m", "clip", "--instrumental", "-o", "-"},
		{"music", "-mclip", "--format=wav", "--", "--help"},
	} {
		if _, _, err := runCLI(args...); err == nil || isUsageError(err) || err.Error() != missingAPIKeyMessage {
			t.Errorf("%v: want missing key error, got %v", args, err)
		}
	}
}
