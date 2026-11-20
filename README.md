# generate_media

Generate images, videos, speech, and music via Gemini. The Go CLI is a fast, standalone binary with no runtime dependencies. Images are the default: image mode accepts PNG, JPEG, GIF, WebP, HEIC, HEIF, PDF, and video inputs and writes PNG or JPEG output. Add `--video` to create an MP4 with Gemini Omni from a prompt, images, or uploaded video. Optionally produce a transparent PNG using difference matting. Use `generate_media tts` for text to speech with Gemini 3.8 Flash TTS and Flash-Lite TTS. Use `generate_media music` for songs and clips with Lyria 3.5.

## How transparency works

1. Generates (or extracts) an object on a **white** background
2. Edits that image to have a **black** background
3. Computes the alpha channel from the difference between the two

## Go CLI

Requires Go 1.23 or later. The CLI is built on [Cobra](https://github.com/spf13/cobra); the first build downloads it.

```bash
export GEMINI_API_KEY="your-key"
./build.sh
./dist/generate_media "a cat with a hat"
```

Run `./build.sh` again after changing Go source files. It builds the Go CLI at `dist/generate_media` and retains `dist/generate` and `dist/nanobanana` aliases for existing callers. `./go/build.sh` remains available when you only need its standalone output at `go/dist/generate_media`; that directory also gets both aliases. To put the new command on your PATH:

```bash
mkdir -p "$HOME/.local/bin"
ln -s "$PWD/dist/generate_media" "$HOME/.local/bin/generate_media"
```

To bake an API key from `.env` into the binary, put this in the repository's `.env` (or `go/.env`):

```dotenv
GEMINI_API_KEY="your-key"
```

Then run `./build.sh`. The resulting `dist/generate_media` runs without `.env` or an API key environment variable on the destination machine.

Build-time precedence is `NANOBANANA_BUNDLE_KEY` from the environment, then `GEMINI_API_KEY` from the environment, then the first existing file of `go/.env` and the repository `.env`. Within that file, `NANOBANANA_BUNDLE_KEY` takes priority over `GEMINI_API_KEY`. Quoted values, comments, and `export KEY=...` are supported; values are read literally without shell execution. `.env` files are ignored by Git.

You can also supply the key directly through the build environment:

```bash
NANOBANANA_BUNDLE_KEY="your-key" ./build.sh
```

`GEMINI_API_KEY` takes priority at runtime. The build writes the bundled key only to a temporary generated Go source file, which is removed after the build.

## Usage

```bash
./dist/generate_media "a cat with a hat"                          # image → output.png
./dist/generate_media "a glass vase" -o vase.png --transparent    # transparent PNG (two API calls)
./dist/generate_media "a phone wallpaper" -a 9:16 -m pro          # aspect ratio + model
./dist/generate_media -i photo.jpg "remove the background"        # edit an image
./dist/generate_media -i photo.jpg "the vase" --transparent       # extract with transparency
./dist/generate_media -i a.png,b.png "combine into one scene"     # multiple input images
./dist/generate_media "a cat waving" --video --duration 15         # 15-second video → output.mp4
./dist/generate_media -i photo.jpg "make this scene move" --video -a 16:9 --resolution 720p -o scene.mp4
./dist/generate_media --video --first-frame start.png --last-frame end.png "pan from sunrise to dusk" -o transition.mp4
./dist/generate_media --video -i source.mp4 "make the lighting warmer" --task edit -o warm.mp4
./dist/generate_media --video -i source.mp4,character.png "continue with this character" --task extend -o extended.mp4
./dist/generate_media --video --session clip.json "a cat waves" -o cat.mp4
./dist/generate_media --video --session clip.json "make the cat orange" -o orange-cat.mp4
./dist/generate_media --video --session clip.json "continue the action" --task extend -o longer-cat.mp4
./dist/generate_media "a city at dusk" --image-size 4K --thinking high --search both --text
./dist/generate_media "continue the composition" --session scene.json -i sketch.png
```

Options can go before or after the prompt. Run `generate_media --help` (or `generate_media help`) for grouped options, `generate_media --version` for the build version, and `generate_media completion zsh` (or `bash`, `fish`, `powershell`) for a shell completion script. Because `help`, `tts`, `music`, `batch`, and `completion` are commands, pass a prompt that is one of those words, or that starts with a dash, after `--`: `generate_media -- help`. Mistakes in how a command is invoked exit with status 2; failed generations and API errors exit with status 1.

Video uses Gemini Omni 1.2 Flash by default, with synchronized speech, music, and sound effects described in the prompt. The default aspect ratio is `16:9` and the default resolution is `720p`. Use `--aspect 9:16` for portrait video. Set `--duration` to a whole number from 3 to 15 seconds for generation or editing. First-and-last-frame interpolation, extension, and Omni 1.1 support 3 to 10 seconds per turn. Omni infers the task from the prompt and inputs when you omit `--task`. Video mode accepts image and video inputs through `-i`. [Gemini Omni guide](https://ai.google.dev/gemini-api/docs/omni)

`--first-frame` and `--last-frame` send the starting and ending images before any `-i` references. The ending frame requires a starting frame. The CLI describes frame roles in the prompt for Omni 1.2 and uses structured role declarations for Omni 1.1. In Omni 1.2, edit and extension use one input video or a previous interaction as the source, with optional reference images.

Video `--session` creates or resumes a separate JSON session containing the model and last stored interaction ID. Existing sessions keep their original model when you omit `--model`. Keep the same session path and use a different `-o` path to retain older outputs. Use `--previous-interaction-id` to continue a stored interaction directly. When you also pass a session file, the ID must match the last stored interaction unless the session is new. Sessions, direct continuations, streams, and background jobs store interactions server-side. Interaction history can expire. Specify `--aspect`, `--resolution`, `--duration`, and `--task` on each turn as needed. Use `--response-json` to save the raw video API response.

Use `--stream` to see progress during generation or retrieval. When combined with `--response-json`, the CLI saves the server's events as a JSON array. Use `--background` to submit a job and print its interaction ID. Retrieve the result with `generate_media --get-interaction-id ID -o clip.mp4`, or cancel a running job with `generate_media --cancel-interaction-id ID`. Gemini API requests use URI delivery by default. Use `--delivery inline` to receive the video in the response.

On the Gemini API, a new video can use up to six image references and three video references. Clips longer than 10 seconds can use one video reference. Each video reference can be up to three seconds long. Extension can produce up to 30 seconds total. Google applies regional restrictions to editing or extending uploaded videos. [Gemini Omni limitations](https://ai.google.dev/gemini-api/docs/omni#limitations)

### Gemini Omni on Google Cloud

Use `--vertex-project PROJECT` with a project that has the Vertex AI API enabled. Authenticate with `gcloud auth login` or supply `GOOGLE_CLOUD_ACCESS_TOKEN`. Pass the same project when resuming a session or retrieving a job. Cloud input media can be local files within the inline request budget or `gs://` object URIs. Cloud responses default to inline delivery. Set `--gcs-output gs://bucket/prefix/` for URI delivery and the CLI will download the resulting MP4 from Cloud Storage. [Google Cloud video guide](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/video/overview)

```bash
./dist/generate_media --vertex-project my-project --seed 42 --label team=media \
  --duration 15 --resolution 720p --session clip.json "A paper boat drifting down a stream"
./dist/generate_media --vertex-project my-project --task upscale --session clip.json \
  --resolution 4k --gcs-output gs://my-bucket/renders/ -o boat-4k.mp4
./dist/generate_media --vertex-project my-project --task upscale \
  -i gs://my-bucket/draft.mp4 --resolution 1080p -o draft-1080p.mp4
```

Standalone upscaling uses a 360p or 720p input up to 15 seconds long. Choose 720p or 1080p from a 360p source, or 1080p or 4K from a 720p source. Cloud editing and extension accept up to two reference images. Use `--vertex-traffic shared` for pay-as-you-go or `--vertex-traffic dedicated` with Provisioned Throughput. [Upscaling guide](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/video/upscale-videos) [Provisioned Throughput](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/provisioned-throughput/use-provisioned-throughput)

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
| `--text` | Request text and images in image mode |
| `--session <path>` | Save or resume an image or video JSON conversation; each mode has its own format |
| `--response-json <path>` | Save the raw image or video API response |
| `--duration <seconds>` | Video length in whole seconds (`8` or `8s`), up to 15 for supported Omni 1.2 tasks |
| `--task <mode>` | Explicit video mode: `text_to_video`, `image_to_video`, `reference_to_video`, `edit`, `extend`, or `upscale` (Google Cloud) |
| `--first-frame <path>` | Starting image for video interpolation |
| `--last-frame <path>` | Ending image for video interpolation; requires `--first-frame` |
| `--previous-interaction-id <id>` | Continue a stored Omni interaction without a session file |
| `--delivery <mode>` | Receive video as `inline` data or a `uri` |
| `--stream` | Show progress during video generation or retrieval |
| `--background` | Submit video generation and print the interaction ID |
| `--get-interaction-id <id>` | Wait for an interaction and save its video |
| `--cancel-interaction-id <id>` | Cancel an in-progress video interaction |
| `--vertex-project <id>` | Use Google Cloud for video generation |
| `--vertex-traffic <mode>` | Google Cloud traffic mode, `shared` or `dedicated` |
| `--gcs-output <uri>` | Cloud Storage output prefix for URI delivery |
| `--seed <integer>` | Set the generation seed on Google Cloud |
| `--label <key=value>` | Add a Google Cloud request label (repeatable) |
| `--transparent` | Produce a transparent PNG via difference matting (two API calls) |
| `--save-intermediates` | Save white/black intermediate images |
| `--timings` | Report API, local processing, and total time (Go CLI) |
| `-h, --help` | Show help for the command |
| `--version` | Show the build version |

## Models

| Alias | Model ID | |
|---|---|---|
| `flash` | `gemini-nano-banana-2.1` | Nano Banana 2.1 (default) |
| `flash-lite` | `gemini-3.1-flash-lite-image` | Recommended for speed |
| `pro` | `gemini-3-pro-image` | Nano Banana Pro, highest quality |
| `flash-2` | `gemini-2.5-flash-image` | Legacy |
| `omni` | `gemini-omni-1.2-flash` | Default for `--video` |
| `omni-1.1` | `gemini-omni-1.1-flash` | Previous video model |

Use Lite with `./dist/generate_media "a cat with a hat" -m flash-lite` when speed matters.

For example, `./dist/generate_media "a cat with a hat" -m flash-lite --timings` measures where time is spent. Direct image generation uses one API call. Transparency uses two sequential calls because the black-background edit depends on the white-background result. Video generation can take multiple requests while Gemini prepares and serves the MP4.

Image generation saves every returned image: the first at `-o`, then `-2`, `-3`, and so on. If a response contains text or multiple images, an ordered Markdown result is saved beside the output. Search grounding creates a `-grounding.html` attribution file and prints source page URLs. Nano Banana 2.1 accepts up to 14 input images. Video inputs can be MP4, M4V, MPEG, MPG, MOV, AVI, FLV, WebM, WMV, or 3GP. In image mode, `--session` stores the model's original response content so later turns can reuse its thought signatures; it cannot be combined with `--transparent`. Uploaded Files API inputs expire after 48 hours; start a new image session if its earlier video, PDF, or large-image file references have expired.

## Text to speech

`generate_media tts` uses [Gemini 3.8 TTS](https://ai.google.dev/gemini-api/docs/speech-generation) through the Interactions API with the same `GEMINI_API_KEY`.

```bash
./dist/generate_media tts "Have a wonderful day!" -v Puck -s "cheerful and friendly"      # → output.wav
./dist/generate_media tts "Wait... <short pause> did you hear that? <gasp>" -s "whispered, nervous"
./dist/generate_media tts -f chapter.txt -m flash-lite -o chapter.wav --stream --play
./dist/generate_media tts --speaker Joe=Puck --speaker Jane=Kore -o chat.wav \
  $'Joe: So the launch is Thursday |oh hmm| are we ready?\nJane (calm): We are.'
./dist/generate_media tts "Hei, hvordan går det?" --language nb-NO --format mulaw -o hei.ulaw
./dist/generate_media tts "Straight to a player" --stream -o - | ffplay -nodisp -autoexit -
```

| Alias | Model ID | |
|---|---|---|
| `flash` | `gemini-3.8-flash-tts` | Most expressive, 130+ languages (default) |
| `flash-lite` | `gemini-3.8-flash-lite-tts` | Faster and cheaper, 100+ languages |

The text is read verbatim, so put delivery instructions in `--style` rather than the text. Inside the text, use English inline tags such as `<sigh>`, `<laugh>`, `<gasp>`, `<whispering>`, `<breath>`, `<short pause>`, and `<long pause>`; capitalize a WORD for emphasis. The models detect the language automatically; `--language` is an optional hint.

| Flag | Description |
|---|---|
| `-o, --output <path>` | `.wav` (default `output.wav`), raw `.pcm`/`.raw`/`.l16`, `.ulaw`/`.mulaw`, `.alaw`, or `-` for WAV on stdout |
| `-m, --model <name>` | `flash` (default), `flash-lite`, or a full model ID |
| `-v, --voice <id>` | Prebuilt voice (default `Kore`), Extended Voice Library ID, or custom `voice_…`/`voicekey_…` |
| `-s, --style <text>` | Delivery direction, such as `"warm and enthusiastic"` |
| `-f, --file <path>` | Read the text from a file, or `-` for stdin |
| `--speaker <NAME=VOICE>` | Repeat for dialogue; lines start with `NAME:` or `NAME (style):` |
| `--per-turn` | Synthesize dialogue one turn at a time |
| `--gap <duration>` | Silence between per-turn lines (default `250ms`) |
| `--language <code>` | BCP-47 language hint |
| `--format <encoding>` | `pcm` (24 kHz 16-bit, default), `mulaw`, or `alaw` (8 kHz G.711) |
| `--seed <n>` | Sampling seed for more repeatable takes |
| `--stream` | Stream audio while it is generated; `--timings` reports time to first audio |
| `--play` | Play the saved WAV (`afplay`, `ffplay`, `paplay`, or `aplay`) |
| `--response-json <path>` | Save the raw API response (not with `--stream`) |
| `--timings` | Report API time and token usage |

Dialogue with exactly two prebuilt voices uses Gemini's native multi-speaker mode in one request, which supports overlapping reactions written in pipes (`|mm-hmm|`). More than two speakers, custom voices, or `--per-turn` synthesize each turn separately (in parallel unless streaming) and join them with `--gap` silence. Output is mono: 24 kHz for PCM and 8 kHz for μ-law and A-law. Gemini fixes these sample rates. A WAV output keeps the encoding chosen with `--format`. Requests are limited to 8,192 input tokens and 16,384 output tokens (about 11 minutes of audio).

### Voices

```bash
./dist/generate_media tts voices --builtin                     # the 30 prebuilt voices, offline
./dist/generate_media tts voices --language en-GB --gender female --search "calm narrator"
./dist/generate_media tts voice design --description "A warm, thoughtful astronomer in his late 60s with a gentle British accent" --name Astronomer
./dist/generate_media tts voice remix --base Puck --description "slightly older, measured audiobook pacing"
./dist/generate_media tts voice clone --source me.wav --consent consent.wav --name "My voice"
./dist/generate_media tts voice get voice_123 -o sample.wav
./dist/generate_media tts voice delete voice_123
```

`tts voices` searches the prebuilt voices, the Extended Voice Library of more than 2,000 voices, and your own voices. It accepts `--language`, `--region`, `--gender`, `--pitch`, `--type`, `--accent`, `--persona`, `--context`, `--search`, `--page-size`, `--page-token`, `--all`, and `--json`. Voice design and remix create stored `voice_…` voices and save a sample WAV. Cloning needs 10–30 seconds of clean speech plus a recording of the same adult speaker reading Google's consent statement; only clone voices you have permission to use. `--no-store` returns a temporary `voicekey_…` that expires after 7 days. Custom voices require a paid-tier project, which can store up to 200 voices; stored voices expire one year after last use. All generated speech carries a SynthID watermark.

## Music

`generate_media music` uses [Lyria 3.5](https://ai.google.dev/gemini-api/docs/music-generation) through the Interactions API with the same `GEMINI_API_KEY`.

```bash
./dist/generate_media music "An upbeat synth-pop song about a road trip at dawn"          # → output.mp3
./dist/generate_media music "Lo-fi hip hop beat, 80 BPM, mellow Rhodes" -m clip --instrumental
./dist/generate_media music "Dreamy indie pop, female vocals" --lyrics lyrics.txt -o dream.wav
./dist/generate_media music -i sunset.jpg "An ambient track inspired by this image" --play
./dist/generate_media music -f brief.txt -o - | ffplay -nodisp -autoexit -
```

| Alias | Model ID | |
|---|---|---|
| `song` | `lyria-3.5` | Full songs with verses, choruses, and bridges (default) |
| `clip` | `lyria-3-clip-preview` | 30-second clips, loops, and previews |

Lyria takes every instruction from the prompt, so describe the genre, mood, instruments, tempo, key, and length there. Tag sections in lyrics with `[Verse 1]`, `[Chorus]`, and `[Bridge]`. To control timing, write one segment per line, such as `[0:00 - 0:10] Intro: soft piano`. Lyrics follow the language of the prompt. `--lyrics` appends your lyrics to the prompt and `--instrumental` asks for a track without vocals.

| Flag | Description |
|---|---|
| `-o, --output <path>` | `.mp3` (default `output.mp3`) or `.wav`, or `-` for audio on stdout |
| `-m, --model <name>` | `song` (default), `clip`, or a full model ID |
| `-i, --input <paths>` | Up to 10 reference images (PNG, JPEG, GIF, WebP, HEIC, or HEIF); repeat or comma-separate |
| `-f, --file <path>` | Read the prompt from a file, or `-` for stdin |
| `--lyrics <path>` | Sing the lyrics in this file, or `-` for stdin |
| `--instrumental` | Ask for a track without vocals |
| `--format <type>` | `mp3` or `wav`; defaults to the output extension |
| `--play` | Play the saved audio (`afplay`, `ffplay`, `paplay`, or `aplay`) |
| `--response-json <path>` | Save the raw API response |
| `--timings` | Report API time and token usage |

Lyria returns the lyrics and section structure it wrote for the track; they are saved beside the audio as `<output>-lyrics.txt`. Reference images share a 14 MiB request budget. Music generation is a single turn: to change a track, adjust the prompt and generate again. Results vary between calls with the same prompt. Requests that imitate a specific artist's voice or reproduce copyrighted lyrics are blocked, and all output carries Google's SynthID watermark.

## Batch image generation

Use [Gemini Batch API](https://ai.google.dev/gemini-api/docs/batch-api) for requests that can run asynchronously. Each JSONL line has a unique `key` and a full `GenerateContentRequest` in `request`:

```bash
printf '%s\n' '{"key":"scene-1","request":{"contents":[{"role":"user","parts":[{"text":"A mountain at dawn"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}}}}' > requests.jsonl
./dist/generate_media batch submit requests.jsonl --name mountain-scenes
./dist/generate_media batch status batches/ID
./dist/generate_media batch download batches/ID --output-dir results
./dist/generate_media batch cancel batches/ID
```

Use the batch name returned by `submit` in place of `batches/ID`. Download saves the raw results JSONL and extracts visible images and text. Run `./dist/generate_media batch --help` or `./dist/generate_media batch submit --help` for command details.

On an Apple M2, ten warm `--help` runs had median startup times of **5 ms for Go** and **543 ms for Python** with dependencies already installed. The stripped Go binary is about **9 MiB**. A synthetic 1024×1024 difference-matting benchmark, including PNG decoding and encoding, took **28 ms/op**. These local measurements exclude network and model latency; they are not end-to-end generation speed claims.

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
