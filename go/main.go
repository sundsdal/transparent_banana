package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var bundledAPIKey string

const defaultModel = "gemini-3.1-flash-image"

var models = map[string]string{
	"flash":      defaultModel,
	"flash-lite": "gemini-3.1-flash-lite-image",
	"pro":        "gemini-3-pro-image",
	"flash-2":    "gemini-2.5-flash-image",
}

const help = `nanobanana — Generate and edit images via Gemini.

Usage: nanobanana [options] <prompt> [options]

Options:
  -i, --input <paths>       Input PNG, JPEG, or GIF files (comma-separated)
  -o, --output <path>       Output PNG or JPEG (default: output.png)
  -m, --model <name>        Alias or full model ID (default: flash)
  -a, --aspect <ratio>      Output aspect ratio
      --transparent        Produce a transparent PNG using difference matting
      --save-intermediates Save white/black images beside the output
      --timings            Report API and local processing durations
  -h, --help               Show this help

Models:
  flash       gemini-3.1-flash-image       Nano Banana 2 (default)
  flash-lite  gemini-3.1-flash-lite-image  Nano Banana 2 Lite (fastest)
  pro         gemini-3-pro-image          Nano Banana Pro
  flash-2     gemini-2.5-flash-image       Legacy

Aspect ratios:
  1:1 1:4 1:8 2:3 3:2 3:4 4:1 4:3 4:5 5:4 8:1 9:16 16:9 21:9

Examples:
  nanobanana "a cat with a hat" -m flash-lite
  nanobanana -i photo.jpg "remove the background"
  nanobanana "a glass vase" --transparent -o vase.png
  nanobanana -i a.png,b.png "combine these into one scene"

GEMINI_API_KEY overrides the bundled key. Transparent output needs two
sequential API calls; direct generation or editing needs one.
`

type options struct {
	prompt, input, output, model, aspect      string
	transparent, intermediates, timings, help bool
}

// Parse flags before or after the prompt, matching the existing Python CLI.
func parseArgs(args []string) (options, error) {
	o := options{output: "output.png", model: defaultModel}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "-h", "--help", "--transparent", "--save-intermediates", "--timings":
			if hasValue {
				return o, fmt.Errorf("%s does not take a value", name)
			}
			switch name {
			case "-h", "--help":
				o.help = true
			case "--transparent":
				o.transparent = true
			case "--save-intermediates":
				o.intermediates = true
			case "--timings":
				o.timings = true
			}
		case "-i", "--input", "-o", "--output", "-m", "--model", "-a", "--aspect":
			if !hasValue {
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "-") {
					return o, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if value == "" {
				return o, fmt.Errorf("%s requires a nonempty value", name)
			}
			switch name {
			case "-i", "--input":
				o.input = value
			case "-o", "--output":
				o.output = value
			case "-m", "--model":
				o.model = value
			case "-a", "--aspect":
				o.aspect = value
			}
		default:
			return o, fmt.Errorf("unknown option: %s", name)
		}
	}
	if o.help {
		return o, nil
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return o, errors.New("provide one quoted, nonempty prompt")
	}
	o.prompt = positional[0]
	if id, ok := models[o.model]; ok {
		o.model = id
	}
	if strings.ContainsAny(o.model, "/?# \t\n") {
		return o, errors.New("invalid model ID")
	}
	if o.aspect != "" && !strings.Contains(" 1:1 1:4 1:8 2:3 3:2 3:4 4:1 4:3 4:5 5:4 8:1 9:16 16:9 21:9 ", " "+o.aspect+" ") {
		return o, fmt.Errorf("unsupported aspect ratio: %s", o.aspect)
	}
	output := strings.ToLower(o.output)
	if !strings.HasSuffix(output, ".png") && !strings.HasSuffix(output, ".jpg") && !strings.HasSuffix(output, ".jpeg") {
		return o, errors.New("output must have a .png, .jpg, or .jpeg extension")
	}
	if o.transparent && !strings.HasSuffix(output, ".png") {
		return o, errors.New("transparent output requires a .png file")
	}
	return o, nil
}

type generator interface {
	generate(context.Context, string, string, []imageData, string) (imageData, error)
}

func run(ctx context.Context, o options, client generator, out io.Writer) error {
	start := time.Now()
	var apiTime time.Duration
	var images []imageData
	if o.input != "" {
		for _, path := range strings.Split(o.input, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				return errors.New("input paths cannot be empty")
			}
			img, err := loadImage(path)
			if err != nil {
				return err
			}
			images = append(images, img)
		}
	}
	call := func(prompt string, refs []imageData, aspect string) (imageData, error) {
		t := time.Now()
		img, err := client.generate(ctx, o.model, prompt, refs, aspect)
		apiTime += time.Since(t)
		return img, err
	}
	var result imageData
	if o.transparent {
		prompt := o.prompt + ". On a pure solid white #FFFFFF background"
		if len(images) > 0 {
			prompt = "This image contains " + o.prompt + ". Remove everything from the scene except " + o.prompt + ". Place the isolated object on a plain, pure white #FFFFFF background with no shadows, no reflections, and no other elements. Do not change the object itself in any way — preserve its exact colors, shape, size, and details."
		}
		fmt.Fprintln(out, "Step 1/3: Generating object on white background...")
		white, err := call(prompt, images, o.aspect)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Step 2/3: Editing background to black...")
		black, err := call("Change the white background to a solid pure black #000000 background. Keep everything else exactly unchanged.", []imageData{white}, o.aspect)
		if err != nil {
			return err
		}
		if o.intermediates {
			wp, bp := intermediatePaths(o.output)
			if err := saveImage(wp, white); err != nil {
				return err
			}
			if err := saveImage(bp, black); err != nil {
				return err
			}
			fmt.Fprintf(out, "Saved intermediates: %s, %s\n", wp, bp)
		}
		fmt.Fprintln(out, "Step 3/3: Extracting alpha via difference matting...")
		result, err = matte(white, black)
		if err != nil {
			return err
		}
	} else {
		if len(images) > 0 {
			fmt.Fprintln(out, "Editing image...")
		} else {
			fmt.Fprintln(out, "Generating image...")
		}
		var err error
		result, err = call(o.prompt, images, o.aspect)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := saveImage(o.output, result); err != nil {
		return err
	}
	fmt.Fprintf(out, "Done! Image saved to: %s\n", o.output)
	if o.timings {
		total := time.Since(start)
		fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
	}
	return nil
}

func resolveAPIKey() string {
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		return key
	}
	if bundledAPIKey != "" {
		return bundledAPIKey
	}
	return os.Getenv("NANOBANANA_BUNDLE_KEY")
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(2)
	}
	if o.help {
		fmt.Print(help)
		return
	}
	key := resolveAPIKey()
	if key == "" {
		fmt.Fprintln(os.Stderr, "Error: GEMINI_API_KEY environment variable is required")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &geminiClient{apiKey: key, httpClient: &http.Client{Timeout: 5 * time.Minute}}
	if err := run(ctx, o, client, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
