# generate

Generate images and videos via Gemini. The Go CLI is a fast, standalone binary with no runtime dependencies. Images are the default: image mode accepts PNG, JPEG, GIF, WebP, HEIC, HEIF, PDF, and video inputs and writes PNG or JPEG output. Add `--video` to create an MP4 with Gemini Omni from a prompt, images, or uploaded video. Optionally produce a transparent PNG using difference matting.

## How transparency works

1. Generates (or extracts) an object on a **white** background
2. Edits that image to have a **black** background
3. Computes the alpha channel from the difference between the two

## Go CLI

Requires Go 1.23 or later.

```bash
export GEMINI_API_KEY="your-key"
./build.sh
./dist/generate "a cat with a hat"
```

Run `./build.sh` again after changing Go source files. It builds the Go CLI at `dist/generate` and retains `dist/nanobanana` for existing callers. `./go/build.sh` remains available when you only need its standalone output at `go/dist/generate`. To put the new command on your PATH:

```bash
mkdir -p "$HOME/.local/bin"
ln -s "$PWD/dist/generate" "$HOME/.local/bin/generate"
```

To bake an API key from `.env` into the binary, put this in the repository's `.env` (or `go/.env`):

```dotenv
GEMINI_API_KEY="your-key"
```

Then run `./build.sh`. The resulting `dist/generate` runs without `.env` or an API key environment variable on the destination machine.

Build-time precedence is `NANOBANANA_BUNDLE_KEY` from the environment, then `GEMINI_API_KEY` from the environment, then the first existing file of `go/.env` and the repository `.env`. Within that file, `NANOBANANA_BUNDLE_KEY` takes priority over `GEMINI_API_KEY`. Quoted values, comments, and `export KEY=...` are supported; values are read literally without shell execution. `.env` files are ignored by Git.

You can also supply the key directly through the build environment:

```bash
NANOBANANA_BUNDLE_KEY="your-key" ./build.sh
```

`GEMINI_API_KEY` takes priority at runtime. The build writes the bundled key only to a temporary generated Go source file, which is removed after the build.

## Usage

```bash
./dist/generate "a cat with a hat"                          # image → output.png
./dist/generate "a glass vase" -o vase.png --transparent    # transparent PNG (two API calls)
./dist/generate "a phone wallpaper" -a 9:16 -m pro          # aspect ratio + model
./dist/generate -i photo.jpg "remove the background"        # edit an image
./dist/generate -i photo.jpg "the vase" --transparent       # extract with transparency
./dist/generate -i a.png,b.png "combine into one scene"     # multiple input images
./dist/generate "a cat waving" --video --duration 8          # 8-second video → output.mp4
./dist/generate -i photo.jpg "make this scene move" --video -a 16:9 --resolution 720p -o scene.mp4
./dist/generate --video --first-frame start.png --last-frame end.png "pan from sunrise to dusk" -o transition.mp4
./dist/generate --video -i source.mp4 "make the lighting warmer" --task edit -o warm.mp4
./dist/generate --video -i source.mp4,character.png "continue with this character" --task extend -o extended.mp4
./dist/generate --video --session clip.json "a cat waves" -o cat.mp4
./dist/generate --video --session clip.json "make the cat orange" -o orange-cat.mp4
./dist/generate --video --session clip.json "continue the action" --task extend -o longer-cat.mp4
./dist/generate "a city at dusk" --image-size 4K --thinking high --search both --text
./dist/generate "continue the composition" --session scene.json -i sketch.png
```

Video uses [Gemini Omni](https://ai.google.dev/gemini-api/docs/omni). Its default aspect ratio is `16:9` and default resolution is `720p`; use `--aspect 9:16` for portrait video. Set `--duration` to a whole number from 3 to 10 seconds (for example, `8` or `8s`), or omit it to use the API default. By default Omni infers whether to generate, edit, or extend from the prompt and inputs. Use `--task` only when you need to select that mode explicitly; the task can constrain the model. Image, video, and reference inputs are accepted through `-i`; PDFs and audio files are not accepted in video mode. `--transparent` and `--save-intermediates` apply only to images.

`--first-frame` and `--last-frame` bind images to the starting and ending frame; the ending frame requires a starting frame. They are sent before any `-i` references. A prompt with only `-i` retains Omni's usual role inference. With frame flags or an explicit reference/edit/extend task, the CLI adds Omni source and reference declarations such as `[# Sources <FIRST_FRAME>@Image1]` and `[# References <IMAGE_REF_0>@Image2]`. Image and video numbers count separately. For an uploaded video with `--task edit` or `--task extend`, the first video is the source; when resuming a stored interaction, that previous video remains the source and uploaded clips are references.

Video `--session` creates or resumes a separate JSON session containing the model and last stored interaction ID. Each successful turn writes a new MP4 and then advances the session; keep the same session path and use a different `-o` path to retain older outputs. You can also resume a stored interaction with `--previous-interaction-id`; when used with a session, its ID must match the session's current ID (or the session must be new). The CLI prints the new interaction ID. Session turns and direct ID continuations set `store=true`; one-off video requests use `store=false`, so they cannot be resumed later by ID. Session files do not preserve `--aspect`, `--resolution`, `--duration`, or `--task`: set them on each turn when needed. Server-side interaction history can expire, so a local session file cannot guarantee indefinite continuation. Use `--response-json` to save the raw video API response beside the MP4.

Gemini limits uploaded source videos for editing or extension to 10 seconds; generated videos continued with a session or previous interaction ID are exempt from that upload limit. Extension appends to the end of a clip and supports up to 40 seconds total. Video references support up to three clips of three seconds each, and their audio is ignored. Gemini currently disallows editing or extending uploaded videos in the EEA, Switzerland, and the UK; editing or extending videos generated by Gemini is supported there. The CLI leaves video duration and regional eligibility checks to Gemini. See the [Omni limitations](https://ai.google.dev/gemini-api/docs/omni#limitations) for current restrictions.

## Options

| Flag | Description |
|---|---|
| `-i, --input <paths>` | Repeat or comma-separate inputs: images and video for either mode; PDF for image mode only |
| `-o, --output <path>` | Output file path: PNG/JPEG for images (default: `output.png`), MP4 for video (default: `output.mp4`) |
| `-m, --model <name>` | Model alias or full ID (default: `flash`) |
| `-a, --aspect <ratio>` | Aspect ratio (e.g. `16:9`, `1:1`, `9:16`) |
| `--video` | Generate an MP4 with Gemini Omni |
| `--resolution <value>` | Image size `1K`/`2K`/`4K`, or video resolution `360p`/`720p`/`1080p`/`4k` |
| `--image-size <value>` | Image size: `1K`, `2K`, or `4K` (`--resolution` also accepts these in image mode) |
| `--thinking <level>` | Nano Banana 2.1 thinking: `minimal`, `medium`, or `high` |
| `--include-thoughts` | Save returned thinking text and images separately |
| `--search <mode>` | Ground 2.1 images with `web`, `image`, `both`, or `none` |
| `--text` | Request text and images; accept text-only answers |
| `--session <path>` | Save or resume an image or video JSON conversation; each mode has its own format |
| `--response-json <path>` | Save the raw image or video API response |
| `--duration <seconds>` | Video length: 3–10 whole seconds (`8` or `8s`) |
| `--task <mode>` | Explicit video mode: `text_to_video`, `image_to_video`, `reference_to_video`, `edit`, or `extend` |
| `--first-frame <path>` | Starting image for video interpolation |
| `--last-frame <path>` | Ending image for video interpolation; requires `--first-frame` |
| `--previous-interaction-id <id>` | Continue a stored Omni interaction without a session file |
| `--transparent` | Produce a transparent PNG via difference matting (two API calls) |
| `--save-intermediates` | Save white/black intermediate images |
| `--timings` | Report API, local processing, and total time (Go CLI) |

## Models

| Alias | Model ID | |
|---|---|---|
| `flash` | `gemini-nano-banana-2.1` | Nano Banana 2.1 (default) |
| `flash-lite` | `gemini-3.1-flash-lite-image` | Recommended for speed |
| `pro` | `gemini-3-pro-image` | Nano Banana Pro, highest quality |
| `flash-2` | `gemini-2.5-flash-image` | Legacy |
| `omni` | `gemini-omni-1.1-flash` | Video; also selected by `--video` |

Use Lite with `./dist/generate "a cat with a hat" -m flash-lite` when speed matters.

For example, `./dist/generate "a cat with a hat" -m flash-lite --timings` measures where time is spent. Direct image generation uses one API call. Transparency uses two sequential calls because the black-background edit depends on the white-background result. Video generation can take multiple requests while Gemini prepares and serves the MP4.

Image generation saves every returned image: the first at `-o`, then `-2`, `-3`, and so on. If a response contains text or multiple images, an ordered Markdown result is saved beside the output. Search grounding creates a `-grounding.html` attribution file and prints source page URLs. Nano Banana 2.1 accepts up to 14 input images. Video inputs can be MP4, M4V, MPEG, MPG, MOV, AVI, FLV, WebM, WMV, or 3GP. In image mode, `--session` stores the model's original response content so later turns can reuse its thought signatures; it cannot be combined with `--transparent`. Uploaded Files API inputs expire after 48 hours; start a new image session if its earlier video, PDF, or large-image file references have expired.

## Batch image generation

Use [Gemini Batch API](https://ai.google.dev/gemini-api/docs/batch-api) for requests that can run asynchronously. Each JSONL line has a unique `key` and a full `GenerateContentRequest` in `request`:

```bash
printf '%s\n' '{"key":"scene-1","request":{"contents":[{"role":"user","parts":[{"text":"A mountain at dawn"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}}}}' > requests.jsonl
./dist/generate batch submit requests.jsonl --name mountain-scenes
./dist/generate batch status batches/ID
./dist/generate batch download batches/ID --output-dir results
./dist/generate batch cancel batches/ID
```

Use the batch name returned by `submit` in place of `batches/ID`. Download saves the raw results JSONL and extracts visible images and text. Run `./dist/generate batch --help` for command details.

On an Apple M2, ten warm `--help` runs had median startup times of **5 ms for Go** and **543 ms for Python** with dependencies already installed. The stripped Go binary was **6.4 MiB**. A synthetic 1024×1024 difference-matting benchmark, including PNG decoding and encoding, took **28 ms/op**. These local measurements exclude network and model latency; they are not end-to-end generation speed claims.

## Aspect ratios

`1:1` `1:4` `1:8` `2:3` `3:2` `3:4` `4:1` `4:3` `4:5` `5:4` `8:1` `9:16` `16:9` `21:9`

Video supports `16:9` and `9:16`.

## TypeScript CLI

The original TypeScript version is also available:

```bash
npm install
npx tsx transparent-banana.ts "a futuristic helmet" -o helmet.png
npx tsx transparent-banana.ts -i photo.jpg "the vase" -o vase.png
```

## Development

```bash
cd go
go test ./...
go test -bench=. -benchmem
```

Remove the root and standalone Go build outputs with `./clean.sh`.

## Python CLI

The original Python CLI remains available for comparison and benchmarking; the root build and clean scripts target Go:

```bash
uv run nanobanana "a cat with a hat"
```
