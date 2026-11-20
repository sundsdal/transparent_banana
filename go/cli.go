package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// version is replaced at build time with -ldflags "-X main.version=...".
var version = "dev"

const missingAPIKeyMessage = "GEMINI_API_KEY environment variable is required"

const shortHelp = `generate_media — Generate images, videos, speech, and music via Gemini.

Usage: generate_media [options] <prompt>
       generate_media tts [options] <text>
       generate_media music [options] <prompt>
       generate_media batch <submit|status|download|cancel> ...

Examples:
  generate_media "a cat with a hat"
  generate_media "a cat waving" --video --duration 15
  generate_media tts "Have a wonderful day!" -v Puck -s cheerful
  generate_media music "An upbeat synth-pop song about a road trip"
Run 'generate_media --help' for full options and examples.
`

const rootUsageHead = `Usage:
  generate_media [options] <prompt>
  generate_media --get-interaction-id <id> [-o <path>]
  generate_media --cancel-interaction-id <id>
  generate_media <command>{{if .HasAvailableSubCommands}}

Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding}} {{.Short}}{{end}}{{end}}{{end}}
`

const rootUsageTail = `
Models:
  flash       gemini-nano-banana-2.1       Nano Banana 2.1 (default)
  flash-lite  gemini-3.1-flash-lite-image  Nano Banana 2 Lite (fastest)
  pro         gemini-3-pro-image           Nano Banana Pro
  flash-2     gemini-2.5-flash-image       Legacy
  omni        gemini-omni-1.2-flash        Default video model
  omni-1.1    gemini-omni-1.1-flash        Previous video model

Image aspect ratios:
  1:1 1:4 1:8 2:3 3:2 3:4 4:1 4:3 4:5 5:4 8:1 9:16 16:9 21:9
Video aspect ratios: 16:9 9:16

Examples:
  generate_media "a cat with a hat" --image-size 2K
  generate_media -i photo.jpg "remove the background"
  generate_media "a glass vase" --transparent -o vase.png
  generate_media -i a.png,b.png "combine these into one scene"
  generate_media "a cat waving" --video --duration 15 --resolution 720p
  generate_media -i photo.jpg "make this scene move" --video -o scene.mp4
  generate_media --video --session clip.json "a cat waves" -o cat.mp4
  generate_media --video --session clip.json "make the cat orange" -o orange-cat.mp4
  generate_media --video --session clip.json --task extend "continue it" -o longer.mp4
  generate_media --video --first-frame start.png --last-frame end.png "pan across the scene"
  generate_media --vertex-project my-project --task upscale --resolution 1080p -i gs://my-bucket/draft.mp4

Reuse a session path for later turns; use a new output path to keep each result.
Image and video sessions use separate JSON files. Specify output settings each
turn; they are not saved in the session. Without --task, Gemini infers the video
task from the prompt.

GEMINI_API_KEY overrides the bundled key. Transparent images need two
sequential API calls; direct image generation or editing needs one.
Use "generate_media -- <prompt>" when the prompt is a command name or starts
with a dash.
`

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// flagValues holds raw flag values that need conversion before they become options.
type flagValues struct {
	o              options
	inputs, labels []string
	seed           string
}

type flagGroup struct {
	title string
	flags *pflag.FlagSet
}

const placeholderAnnotation = "generate_media_placeholder"

// flagGroups defines every generation flag, grouped the way --help presents them.
func (v *flagValues) flagGroups() []flagGroup {
	var groups []flagGroup
	var fs *pflag.FlagSet
	group := func(title string) {
		fs = pflag.NewFlagSet(title, pflag.ContinueOnError)
		fs.SortFlags = false
		groups = append(groups, flagGroup{title, fs})
	}
	placeholder := func(name, value string) {
		_ = fs.SetAnnotation(name, placeholderAnnotation, []string{value})
	}
	str := func(p *string, name, short, value, usage string) {
		fs.StringVarP(p, name, short, "", usage)
		placeholder(name, value)
	}
	list := func(p *[]string, name, short, value, usage string) {
		fs.StringArrayVarP(p, name, short, nil, usage)
		placeholder(name, value)
	}
	flag := func(p *bool, name, usage string) { fs.BoolVar(p, name, false, usage) }
	o := &v.o

	group("General")
	list(&v.inputs, "input", "i", "paths", "Images or videos; image mode also supports PDF\nRepeat or comma-separate paths")
	str(&o.output, "output", "o", "path", "Output PNG/JPEG, or MP4 for video\n(default: output.png/output.mp4)")
	str(&o.model, "model", "m", "name", "Alias or full model ID (default: flash)")
	str(&o.aspect, "aspect", "a", "ratio", "Output aspect ratio")
	str(&o.resolution, "resolution", "", "value", "Image size 1K/2K/4K or video 360p/720p/1080p/4k")
	str(&o.session, "session", "", "path", "Create or resume a conversation in a JSON file")
	str(&o.responseJSON, "response-json", "", "path", "Save the raw API response")
	flag(&o.timings, "timings", "Report API and local processing durations")
	fs.BoolP("help", "h", false, "Show this help")
	fs.Bool("version", false, "Show the version")

	group("Image")
	str(&o.imageSize, "image-size", "", "value", "Image size: 1K, 2K, or 4K")
	str(&o.thinking, "thinking", "", "level", "Image thinking: minimal, medium, or high")
	flag(&o.includeThoughts, "include-thoughts", "Save model thoughts separately when available")
	str(&o.search, "search", "", "mode", "Ground image with web, image, or both search")
	flag(&o.text, "text", "Request text alongside images; accept text-only output")
	flag(&o.transparent, "transparent", "Produce a transparent PNG using difference matting")
	flag(&o.intermediates, "save-intermediates", "Save white/black images beside the output")

	group("Video")
	flag(&o.video, "video", "Generate video with Gemini Omni")
	str(&o.duration, "duration", "", "seconds", "Video length in whole seconds (3–15 for Omni 1.2,\n3–10 for interpolation, extension, and older models)")
	str(&o.task, "task", "", "mode", "Video task: text_to_video, image_to_video,\nreference_to_video, edit, extend, or upscale (Cloud)")
	str(&o.firstFrame, "first-frame", "", "path", "Starting image for video interpolation")
	str(&o.lastFrame, "last-frame", "", "path", "Ending image (requires --first-frame)")
	str(&o.previousInteractionID, "previous-interaction-id", "", "id", "Continue a stored Omni interaction")
	str(&o.delivery, "delivery", "", "mode", "Video delivery: inline or uri (see --gcs-output)")
	flag(&o.stream, "stream", "Show Omni progress during generation or retrieval")

	group("Video jobs")
	flag(&o.background, "background", "Submit a video interaction and print its ID")
	str(&o.getInteractionID, "get-interaction-id", "", "id", "Wait for a stored interaction and save its video")
	str(&o.cancelInteractionID, "cancel-interaction-id", "", "id", "Cancel an in-progress background interaction")

	group("Google Cloud video")
	str(&o.vertexProject, "vertex-project", "", "id", "Use Google Cloud for Omni video")
	str(&o.vertexTraffic, "vertex-traffic", "", "mode", "Google Cloud traffic: shared or dedicated")
	str(&o.gcsOutput, "gcs-output", "", "uri", "Cloud Storage output prefix (gs://bucket/path/)")
	str(&v.seed, "seed", "", "integer", "Reproducible generation on Google Cloud")
	list(&v.labels, "label", "", "key=value", "Google Cloud request label (repeatable)")
	return groups
}

// groupedFlagUsages renders the flag groups with one shared description column.
func groupedFlagUsages(groups []flagGroup) string {
	const column = 28
	var b strings.Builder
	for _, group := range groups {
		fmt.Fprintf(&b, "\n%s:\n", group.title)
		group.flags.VisitAll(func(f *pflag.Flag) {
			left := "      --" + f.Name
			if f.Shorthand != "" {
				left = "  -" + f.Shorthand + ", --" + f.Name
			}
			if value := f.Annotations[placeholderAnnotation]; len(value) > 0 {
				left += " <" + value[0] + ">"
			}
			indent := strings.Repeat(" ", column)
			lines := strings.Split(f.Usage, "\n")
			if len(left)+2 > column {
				b.WriteString(left + "\n" + indent + lines[0] + "\n")
			} else {
				fmt.Fprintf(&b, "%-*s%s\n", column, left, lines[0])
			}
			for _, line := range lines[1:] {
				b.WriteString(indent + line + "\n")
			}
		})
	}
	return b.String()
}

// options converts parsed flags and positional arguments into validated options.
func (v *flagValues) options(flags *pflag.FlagSet, positional []string) (options, error) {
	o := v.o
	var flagErr error
	flags.Visit(func(f *pflag.Flag) {
		if flagErr != nil || f.Value.Type() == "bool" {
			return
		}
		values := []string{f.Value.String()}
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			values = slice.GetSlice()
		}
		for _, value := range values {
			if value == "" {
				flagErr = fmt.Errorf("--%s requires a nonempty value", f.Name)
			} else if strings.HasPrefix(value, "-") && f.Name != "seed" {
				// The next option was consumed as this flag's value.
				flagErr = fmt.Errorf("--%s requires a value", f.Name)
			}
		}
	})
	if flagErr != nil {
		return o, flagErr
	}
	o.input = strings.Join(v.inputs, ",")
	if flags.Changed("seed") {
		seed, err := strconv.ParseInt(v.seed, 10, 32)
		if err != nil {
			return o, errors.New("--seed requires a 32-bit integer")
		}
		n := int32(seed)
		o.seed = &n
	}
	for _, value := range v.labels {
		key, label, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return o, errors.New("--label requires key=value")
		}
		if o.labels == nil {
			o.labels = make(map[string]string)
		}
		o.labels[key] = label
	}
	o.explicitModel = flags.Changed("model")
	if !o.explicitModel {
		o.model = defaultModel
	}
	explicitOutput := flags.Changed("output")
	if !explicitOutput {
		o.output = "output.png"
	}
	err := o.validate(positional, explicitOutput)
	return o, err
}

// parseArgs parses generation arguments without running a command. Flags may
// appear before or after the prompt.
func parseArgs(args []string) (options, error) {
	values := &flagValues{}
	cmd := newGenerateCmd(values, nil)
	if err := cmd.ParseFlags(args); err != nil {
		return values.o, err
	}
	if help, _ := cmd.Flags().GetBool("help"); help {
		values.o.help = true
		return values.o, nil
	}
	return values.options(cmd.Flags(), cmd.Flags().Args())
}

// newGenerateCmd builds the root command: generate_media [options] <prompt>.
func newGenerateCmd(values *flagValues, generate func(context.Context, options, io.Writer) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "generate_media [options] <prompt>",
		Long:          "generate_media — Generate images, videos, speech, and music via Gemini.",
		Version:       version,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && cmd.Flags().NFlag() == 0 {
				_, err := io.WriteString(cmd.OutOrStdout(), shortHelp)
				return err
			}
			o, err := values.options(cmd.Flags(), args)
			if err != nil {
				return usageError{err}
			}
			return generate(cmd.Context(), o, cmd.OutOrStdout())
		},
	}
	groups := values.flagGroups()
	for _, group := range groups {
		cmd.Flags().AddFlagSet(group.flags)
	}
	cmd.SetUsageTemplate(rootUsageHead + groupedFlagUsages(groups) + rootUsageTail)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	return cmd
}

// newRootCmd assembles the full command tree around its two side-effecting dependencies.
func newRootCmd(generate func(context.Context, options, io.Writer) error, batchClient func() (*geminiClient, error), audioClient func() *geminiClient) *cobra.Command {
	root := newGenerateCmd(&flagValues{}, generate)
	root.AddCommand(newTTSCmd(audioClient, os.Stdin), newMusicCmd(audioClient, os.Stdin), newBatchCmd(batchClient))
	return root
}

// audioAPIClient serves the tts and music commands. It may have an empty key:
// commands that do not call the API, such as "tts voices --builtin", work
// without one.
func audioAPIClient() *geminiClient {
	return &geminiClient{apiKey: resolveAPIKey(), httpClient: &http.Client{Timeout: 10 * time.Minute}}
}

func batchAPIClient() (*geminiClient, error) {
	key := resolveAPIKey()
	if key == "" {
		return nil, errors.New(missingAPIKeyMessage)
	}
	return &geminiClient{apiKey: key, httpClient: &http.Client{Timeout: 5 * time.Minute}}, nil
}

// generateMedia builds the API client for validated options and runs the generation.
func generateMedia(ctx context.Context, o options, out io.Writer) error {
	var key string
	if o.vertexProject == "" {
		key = resolveAPIKey()
		if key == "" {
			return errors.New(missingAPIKeyMessage)
		}
	}
	client := &geminiClient{apiKey: key, httpClient: &http.Client{Timeout: 5 * time.Minute}}
	if o.video {
		videoCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		ctx = videoCtx
		client.httpClient = &http.Client{Timeout: 15 * time.Minute}
		if o.vertexProject != "" {
			token, err := vertexAccessToken(ctx)
			if err != nil {
				return err
			}
			client.vertexProject = o.vertexProject
			client.vertexTraffic = o.vertexTraffic
			client.vertexToken = token
		}
	}
	return run(ctx, o, client, out)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cmd, err := newRootCmd(generateMedia, batchAPIClient, audioAPIClient).ExecuteContextC(ctx)
	cancel()
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	var usage usageError
	if errors.As(err, &usage) {
		fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		os.Exit(2)
	}
	os.Exit(1)
}
