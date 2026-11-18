package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var bundledAPIKey string

const defaultModel = "gemini-nano-banana-2.1"
const defaultVideoModel = "gemini-omni-1.2-flash"
const legacyVideoModel = "gemini-omni-1.1-flash"

const shortHelp = `generate_media — Generate images and videos via Gemini.

Usage: generate_media [options] <prompt>
       generate_media batch <submit|status|download|cancel> ...

Examples:
  generate_media "a cat with a hat"
  generate_media "a cat waving" --video --duration 8
Run 'generate_media --help' for full options and examples.
`

var models = map[string]string{
	"flash":      defaultModel,
	"flash-lite": "gemini-3.1-flash-lite-image",
	"pro":        "gemini-3-pro-image",
	"flash-2":    "gemini-2.5-flash-image",
	"omni":       defaultVideoModel,
	"omni-1.1":   legacyVideoModel,
}

const help = `generate_media — Generate images and videos via Gemini.

Usage: generate_media [options] <prompt> [options]
       generate_media batch <submit|status|download|cancel> ... (see generate_media batch --help)

Options:
  -i, --input <paths>       Images or videos; image mode also supports PDF
                            Repeat or comma-separate paths
  -o, --output <path>       Output PNG/JPEG, or MP4 for video (default: output.png/output.mp4)
  -m, --model <name>        Alias or full model ID (default: flash)
  -a, --aspect <ratio>      Output aspect ratio
      --video              Generate video with Gemini Omni
      --resolution <value> Image size 1K/2K/4K or video 360p/720p/1080p/4k
      --image-size <value> Image size: 1K, 2K, or 4K
      --thinking <level>   Image thinking: minimal, medium, or high
      --include-thoughts   Save model thoughts separately when available
      --search <mode>      Ground image with web, image, or both search
      --text               Request text alongside images; accept text-only output
      --session <path>     Create or resume a conversation in a JSON file
      --response-json <path>
                           Save the raw API response
      --duration <seconds> Video length in whole seconds (3–15 for Omni 1.2,
                           3–10 for interpolation, extension, and older models)
      --task <mode>        Video task: text_to_video, image_to_video,
                           reference_to_video, edit, extend, or upscale (Cloud)
      --first-frame <path> Starting image for video interpolation
      --last-frame <path>  Ending image (requires --first-frame)
      --previous-interaction-id <id>
                           Continue a stored Omni interaction
      --delivery <mode>     Video delivery: inline or uri (see --gcs-output)
      --stream              Show Omni progress while generating video
      --background          Submit a video interaction and print its ID
      --get-interaction-id <id>
                           Wait for a stored interaction and save its video
      --cancel-interaction-id <id>
                           Cancel an in-progress background interaction
      --vertex-project <id> Use Google Cloud for Omni video
      --vertex-traffic <mode>
                           Google Cloud traffic: shared or dedicated
      --gcs-output <uri>    Cloud Storage output prefix (gs://bucket/path/)
      --seed <integer>     Reproducible generation on Google Cloud
      --label <key=value>  Google Cloud request label (repeatable)
      --transparent        Produce a transparent PNG using difference matting
      --save-intermediates Save white/black images beside the output
      --timings            Report API and local processing durations
  -h, --help               Show this help

Models:
  flash       gemini-nano-banana-2.1       Nano Banana 2.1 (default)
  flash-lite  gemini-3.1-flash-lite-image  Nano Banana 2 Lite (fastest)
  pro         gemini-3-pro-image          Nano Banana Pro
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
`

type options struct {
	prompt, input, output, model, aspect, resolution, duration string
	imageSize, thinking, search, session, responseJSON         string
	task, firstFrame, lastFrame, previousInteractionID         string
	vertexProject, gcsOutput, delivery, getInteractionID       string
	vertexTraffic, cancelInteractionID                        string
	seed                                                       *int32
	labels                                                     map[string]string
	transparent, intermediates, timings, help, video, text     bool
	includeThoughts, stream, background, explicitModel         bool
}

// Parse flags before or after the prompt, matching the existing Python CLI.
func parseArgs(args []string) (options, error) {
	o := options{output: "output.png", model: defaultModel}
	var explicitOutput bool
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
		case "-h", "--help", "--transparent", "--save-intermediates", "--timings", "--video", "--include-thoughts", "--text", "--stream", "--background":
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
			case "--video":
				o.video = true
			case "--include-thoughts":
				o.includeThoughts = true
			case "--text":
				o.text = true
			case "--stream":
				o.stream = true
			case "--background":
				o.background = true
			}
		case "-i", "--input", "-o", "--output", "-m", "--model", "-a", "--aspect", "--resolution", "--duration", "--image-size", "--thinking", "--search", "--session", "--response-json", "--task", "--first-frame", "--last-frame", "--previous-interaction-id",
			"--vertex-project", "--vertex-traffic", "--gcs-output", "--delivery", "--get-interaction-id", "--cancel-interaction-id", "--seed", "--label":
			if !hasValue {
				i++
				if i >= len(args) || (name != "--seed" && strings.HasPrefix(args[i], "-")) {
					return o, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if value == "" {
				return o, fmt.Errorf("%s requires a nonempty value", name)
			}
			switch name {
			case "-i", "--input":
				if o.input != "" {
					o.input += ","
				}
				o.input += value
			case "-o", "--output":
				o.output = value
				explicitOutput = true
			case "-m", "--model":
				o.model = value
				o.explicitModel = true
			case "-a", "--aspect":
				o.aspect = value
			case "--resolution":
				o.resolution = value
			case "--duration":
				o.duration = value
			case "--image-size":
				o.imageSize = value
			case "--thinking":
				o.thinking = value
			case "--search":
				o.search = value
			case "--session":
				o.session = value
			case "--response-json":
				o.responseJSON = value
			case "--task":
				o.task = value
			case "--first-frame":
				o.firstFrame = value
			case "--last-frame":
				o.lastFrame = value
			case "--previous-interaction-id":
				o.previousInteractionID = value
			case "--vertex-project":
				o.vertexProject = value
			case "--vertex-traffic":
				o.vertexTraffic = value
			case "--gcs-output":
				o.gcsOutput = value
			case "--delivery":
				o.delivery = value
			case "--get-interaction-id":
				o.getInteractionID = value
			case "--cancel-interaction-id":
				o.cancelInteractionID = value
			case "--seed":
				seed, err := strconv.ParseInt(value, 10, 32)
				if err != nil {
					return o, errors.New("--seed requires a 32-bit integer")
				}
				n := int32(seed)
				o.seed = &n
			case "--label":
				key, label, ok := strings.Cut(value, "=")
				if !ok || key == "" {
					return o, errors.New("--label requires key=value")
				}
				if o.labels == nil {
					o.labels = make(map[string]string)
				}
				o.labels[key] = label
			}
		default:
			return o, fmt.Errorf("unknown option: %s", name)
		}
	}
	if o.help {
		return o, nil
	}
	if id, ok := models[o.model]; ok {
		o.model = id
	}
	if o.video || o.vertexProject != "" || o.getInteractionID != "" || o.cancelInteractionID != "" {
		if o.explicitModel && !strings.HasPrefix(o.model, "gemini-omni-") {
			return o, errors.New("--video requires an Omni model")
		}
		if !o.explicitModel {
			o.model = defaultVideoModel
		}
	}
	o.video = strings.HasPrefix(o.model, "gemini-omni-")
	if o.video && (o.imageSize != "" || o.thinking != "" || o.includeThoughts || o.search != "" || o.text) {
		return o, errors.New("image options cannot be used with an Omni video model")
	}
	if strings.ContainsAny(o.model, "/?# \t\n") {
		return o, errors.New("invalid model ID")
	}
	if len(positional) == 1 && strings.TrimSpace(positional[0]) != "" {
		o.prompt = positional[0]
	} else if len(positional) != 0 || !o.video || (o.task != "upscale" && o.getInteractionID == "" && o.cancelInteractionID == "") {
		return o, errors.New("provide one quoted, nonempty prompt")
	}
	validAspects := []string{"1:1", "1:4", "1:8", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "21:9"}
	if o.video {
		if explicitOutput && (o.background || o.cancelInteractionID != "") {
			return o, errors.New("--output applies when saving a video")
		}
		validAspects = []string{"9:16", "16:9"}
	}
	if o.aspect != "" && !containsValue(validAspects, o.aspect) {
		return o, fmt.Errorf("unsupported aspect ratio: %s", o.aspect)
	}
	if collision, err := responseJSONInputCollision(o); err != nil {
		return o, err
	} else if collision {
		return o, errors.New("response JSON path must differ from media inputs")
	}
	if o.video {
		if !explicitOutput {
			o.output = "output.mp4"
		}
		err := o.validateVideoOptions()
		return o, err
	}
	if o.task != "" || o.firstFrame != "" || o.lastFrame != "" || o.previousInteractionID != "" ||
		o.delivery != "" || o.background || o.stream || o.seed != nil || len(o.labels) != 0 || o.gcsOutput != "" || o.vertexTraffic != "" {
		return o, errors.New("video options require an Omni video model")
	}
	if o.resolution != "" {
		if o.imageSize != "" && o.imageSize != o.resolution {
			return o, errors.New("--resolution and --image-size disagree")
		}
		o.imageSize = o.resolution
	}
	if o.imageSize != "" && !containsValue([]string{"1K", "2K", "4K"}, o.imageSize) {
		return o, errors.New("image size must be 1K, 2K, or 4K")
	}
	if o.thinking != "" && !containsValue([]string{"minimal", "medium", "high"}, o.thinking) {
		return o, errors.New("thinking level must be minimal, medium, or high")
	}
	if o.search != "" && !containsValue([]string{"web", "image", "both", "none"}, o.search) {
		return o, errors.New("search mode must be web, image, both, or none")
	}
	if o.session != "" && o.transparent {
		return o, errors.New("--session cannot be combined with --transparent")
	}
	if o.model != defaultModel && (o.thinking != "" || o.includeThoughts || o.search != "") {
		return o, errors.New("thinking and search options require the default Nano Banana 2.1 model")
	}
	if o.duration != "" {
		return o, errors.New("--duration requires video generation")
	}
	output := strings.ToLower(o.output)
	if !strings.HasSuffix(output, ".png") && !strings.HasSuffix(output, ".jpg") && !strings.HasSuffix(output, ".jpeg") {
		return o, errors.New("output must have a .png, .jpg, or .jpeg extension")
	}
	if o.transparent && !strings.HasSuffix(output, ".png") {
		return o, errors.New("transparent output requires a .png file")
	}
	if collision, err := imageArtifactCollision(o, o.session); err != nil {
		return o, err
	} else if collision {
		return o, errors.New("session path must differ from image output artifacts")
	}
	if collision, err := imageArtifactCollision(o, o.responseJSON); err != nil {
		return o, err
	} else if collision {
		return o, errors.New("response JSON path must differ from image output artifacts")
	}
	if collision, err := sameLocalPath(o.responseJSON, o.session); err != nil {
		return o, err
	} else if collision {
		return o, errors.New("response JSON path must differ from session")
	}
	return o, nil
}

func responseJSONInputCollision(o options) (bool, error) {
	if o.responseJSON == "" {
		return false, nil
	}
	inputs := []string{o.firstFrame, o.lastFrame}
	if o.input != "" {
		for _, raw := range strings.Split(o.input, ",") {
			inputs = append(inputs, strings.TrimSpace(raw))
		}
	}
	return pathsCollide(o.responseJSON, inputs...)
}

func imageArtifactCollision(o options, path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	stem := strings.TrimSuffix(o.output, filepath.Ext(o.output))
	artifacts := []string{o.output, stem + ".md"}
	if o.transparent {
		artifacts = append(artifacts, stem+"-white-grounding.html", stem+"-black-grounding.html")
		if o.intermediates {
			white, black := intermediatePaths(o.output)
			artifacts = append(artifacts, white, black)
		}
	} else {
		artifacts = append(artifacts, stem+"-grounding.html")
	}
	if o.includeThoughts {
		artifacts = append(artifacts, stem+"-thoughts.md")
	}
	if collision, err := pathsCollide(path, artifacts...); err != nil || collision {
		return collision, err
	}
	if !o.transparent {
		if collision, err := numberedArtifactCollision(path, stem, filepath.Ext(o.output), 2); err != nil || collision {
			return collision, err
		}
	}
	if o.includeThoughts {
		return numberedArtifactCollision(path, stem+"-thought", ".png", 1)
	}
	return false, nil
}

func numberedArtifactCollision(path, stem, ext string, first int) (bool, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", path, err)
	}
	absolutePrefix, err := filepath.Abs(stem + "-")
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", stem, err)
	}
	number, found := strings.CutPrefix(absolutePath, absolutePrefix)
	if !found || !strings.HasSuffix(number, ext) {
		return false, nil
	}
	number = strings.TrimSuffix(number, ext)
	if number == "" {
		return false, nil
	}
	index, err := strconv.Atoi(number)
	return err == nil && index >= first && strconv.Itoa(index) == number, nil
}

func sameLocalPath(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}
	left, err := filepath.Abs(a)
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", a, err)
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return false, fmt.Errorf("resolve output path %q: %w", b, err)
	}
	return left == right, nil
}

func pathsCollide(path string, others ...string) (bool, error) {
	for _, other := range others {
		collision, err := sameLocalPath(path, other)
		if err != nil || collision {
			return collision, err
		}
	}
	return false, nil
}

func containsValue(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

type generator interface {
	generate(context.Context, string, string, []imageData, string) (imageData, error)
}

type videoGenerator interface {
	generateVideo(context.Context, string, string, []imageData, string, string, string) ([]byte, error)
}

func run(ctx context.Context, o options, client generator, out io.Writer) error {
	if o.video {
		if advanced, ok := client.(videoAdvancedGenerator); ok {
			return runAdvancedVideo(ctx, o, advanced, out)
		}
	}
	if !o.video {
		if advanced, ok := client.(imageAdvancedGenerator); ok {
			return runAdvancedImage(ctx, o, advanced, out)
		}
	}
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
	if o.video {
		videoClient, ok := client.(videoGenerator)
		if !ok {
			return errors.New("video generation is unavailable")
		}
		fmt.Fprintln(out, "Generating video...")
		apiStart := time.Now()
		video, err := videoClient.generateVideo(ctx, o.model, o.prompt, images, o.aspect, o.resolution, o.duration)
		apiTime += time.Since(apiStart)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := saveVideo(o.output, video); err != nil {
			return err
		}
		fmt.Fprintf(out, "Done! Video saved to: %s\n", o.output)
		if o.timings {
			total := time.Since(start)
			fmt.Fprintf(out, "Timing: API %s, local %s, total %s\n", apiTime.Round(time.Millisecond), (total - apiTime).Round(time.Millisecond), total.Round(time.Millisecond))
		}
		return nil
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
	if len(os.Args) == 1 {
		fmt.Print(shortHelp)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "batch" {
		key := resolveAPIKey()
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		client := &geminiClient{apiKey: key, httpClient: &http.Client{Timeout: 5 * time.Minute}}
		if err := client.runBatch(ctx, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}
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
	if key == "" && o.vertexProject == "" {
		fmt.Fprintln(os.Stderr, "Error: GEMINI_API_KEY environment variable is required")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &geminiClient{apiKey: key, httpClient: &http.Client{Timeout: 5 * time.Minute}}
	if o.video {
		videoCtx, timeoutCancel := context.WithTimeout(ctx, 15*time.Minute)
		defer timeoutCancel()
		ctx = videoCtx
		client.httpClient = &http.Client{Timeout: 15 * time.Minute}
		if o.vertexProject != "" {
			client.vertexProject = o.vertexProject
			client.vertexTraffic = o.vertexTraffic
			client.vertexToken, err = vertexAccessToken(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}
		}
	}
	if err := run(ctx, o, client, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
