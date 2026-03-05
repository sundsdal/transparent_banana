# nanobanana

Generate and edit images via Gemini. Optionally produce transparent PNGs using difference matting.

## How transparency works

1. Generates (or extracts) an object on a **white** background
2. Edits that image to have a **black** background
3. Computes the alpha channel from the difference between the two

## Setup

```bash
export GEMINI_API_KEY="your-key"
```

Run directly with [uv](https://docs.astral.sh/uv/) (deps auto-installed):

```bash
./nanobanana "a cat with a hat"
```

### Standalone binary

Build a self-contained binary with PyInstaller:

```bash
./build.sh        # produces dist/nanobanana
./clean.sh        # removes build artifacts
```

To bake an API key into the binary so users don't need to set `GEMINI_API_KEY`:

```bash
NANOBANANA_BUNDLE_KEY="your-key" ./build.sh
```

At runtime, `GEMINI_API_KEY` still takes priority over the bundled key.

## Usage

```bash
nanobanana "a cat with a hat"                          # generate → output.png
nanobanana "a glass vase" -o vase.png --transparent    # transparent PNG
nanobanana "a phone wallpaper" -a 9:16 -m pro          # aspect ratio + model
nanobanana -i photo.jpg "remove the background"        # edit an image
nanobanana -i photo.jpg "the vase" --transparent       # extract with transparency
nanobanana -i a.png,b.png "combine into one scene"     # multiple input images
```

## Options

| Flag | Description |
|---|---|
| `-i, --input <paths>` | Input image(s) to edit (comma-separated) |
| `-o, --output <path>` | Output file path (default: `output.png`) |
| `-m, --model <name>` | Model alias or full ID (default: `flash`) |
| `-a, --aspect <ratio>` | Aspect ratio (e.g. `16:9`, `1:1`, `9:16`) |
| `--transparent` | Enable transparency via difference matting |
| `--save-intermediates` | Save white/black intermediate images |

## Models

| Alias | Model ID | |
|---|---|---|
| `flash` | `gemini-3.1-flash-image-preview` | Default, fast |
| `pro` | `gemini-3-pro-image-preview` | Highest quality |
| `flash-2` | `gemini-2.5-flash-image` | Legacy |

## Aspect ratios

`1:1` `2:3` `3:2` `3:4` `4:3` `4:5` `5:4` `9:16` `16:9` `21:9`

## TypeScript CLI

The original TypeScript version is also available:

```bash
npm install
npx tsx transparent-banana.ts "a futuristic helmet" -o helmet.png
npx tsx transparent-banana.ts -i photo.jpg "the vase" -o vase.png
```
