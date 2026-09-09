# nanobanana

Generate and edit images via Gemini. The Go CLI is a fast, standalone binary with no runtime dependencies. It accepts PNG, JPEG, and GIF input images and writes PNG or JPEG output. Optionally produce a transparent PNG using difference matting.

## How transparency works

1. Generates (or extracts) an object on a **white** background
2. Edits that image to have a **black** background
3. Computes the alpha channel from the difference between the two

## Go CLI

Requires Go 1.23 or later.

```bash
export GEMINI_API_KEY="your-key"
./go/build.sh
./go/dist/nanobanana "a cat with a hat"
```

Run `./go/build.sh` again after changing Go source files. The output binary is `go/dist/nanobanana`.

To bake an API key from `.env` into the binary, put this in the repository's `.env` (or `go/.env`):

```dotenv
GEMINI_API_KEY="your-key"
```

Then run `./go/build.sh`. The resulting `go/dist/nanobanana` runs without `.env` or an API key environment variable on the destination machine.

Build-time precedence is `NANOBANANA_BUNDLE_KEY` from the environment, then `GEMINI_API_KEY` from the environment, then the first existing file of `go/.env` and the repository `.env`. Within that file, `NANOBANANA_BUNDLE_KEY` takes priority over `GEMINI_API_KEY`. Quoted values, comments, and `export KEY=...` are supported; values are read literally without shell execution. `.env` files are ignored by Git.

You can also supply the key directly through the build environment:

```bash
NANOBANANA_BUNDLE_KEY="your-key" ./go/build.sh
```

`GEMINI_API_KEY` takes priority at runtime. The build writes the bundled key only to a temporary generated Go source file, which is removed after the build.

## Usage

```bash
./go/dist/nanobanana "a cat with a hat"                          # generate → output.png
./go/dist/nanobanana "a glass vase" -o vase.png --transparent    # transparent PNG (two API calls)
./go/dist/nanobanana "a phone wallpaper" -a 9:16 -m pro          # aspect ratio + model
./go/dist/nanobanana -i photo.jpg "remove the background"        # edit an image
./go/dist/nanobanana -i photo.jpg "the vase" --transparent       # extract with transparency
./go/dist/nanobanana -i a.png,b.png "combine into one scene"     # multiple input images
```

## Options

| Flag | Description |
|---|---|
| `-i, --input <paths>` | Input image(s) to edit, comma-separated (PNG, JPEG, or GIF) |
| `-o, --output <path>` | Output file path, PNG or JPEG (default: `output.png`) |
| `-m, --model <name>` | Model alias or full ID (default: `flash`) |
| `-a, --aspect <ratio>` | Aspect ratio (e.g. `16:9`, `1:1`, `9:16`) |
| `--transparent` | Produce a transparent PNG via difference matting (two API calls) |
| `--save-intermediates` | Save white/black intermediate images |
| `--timings` | Report API, local processing, and total time (Go CLI) |

## Models

| Alias | Model ID | |
|---|---|---|
| `flash` | `gemini-3.1-flash-image` | Default |
| `flash-lite` | `gemini-3.1-flash-lite-image` | Recommended for speed |
| `pro` | `gemini-3-pro-image` | Nano Banana Pro, highest quality |
| `flash-2` | `gemini-2.5-flash-image` | Legacy |

Use Lite with `./go/dist/nanobanana "a cat with a hat" -m flash-lite` when speed matters.

For example, `./go/dist/nanobanana "a cat with a hat" -m flash-lite --timings` measures where time is spent. Direct generation uses one API call. Transparency uses two sequential calls because the black-background edit depends on the white-background result.

On an Apple M2, ten warm `--help` runs had median startup times of **5 ms for Go** and **543 ms for Python** with dependencies already installed. The stripped Go binary was **6.4 MiB**. A synthetic 1024×1024 difference-matting benchmark, including PNG decoding and encoding, took **28 ms/op**. These local measurements exclude network and model latency; they are not end-to-end generation speed claims.

## Aspect ratios

`1:1` `1:4` `1:8` `2:3` `3:2` `3:4` `4:1` `4:3` `4:5` `5:4` `8:1` `9:16` `16:9` `21:9`

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

Remove only the Go binary output with `./go/clean.sh`.

## Python CLI

The original Python CLI remains available for comparison and benchmarking:

```bash
uv run nanobanana "a cat with a hat"
```
