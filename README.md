# transparent-banana

CLI tool that generates transparent PNGs using Nano Banana and difference matting.

## How it works

1. Generates (or extracts) an object on a **white** background
2. Edits that image to have a **black** background
3. Computes the alpha channel from the difference between the two

This produces clean transparency without relying on traditional background removal — the alpha is derived mathematically from how colors shift between white and black backgrounds.

## Usage

```
npx tsx transparent-banana.ts "a futuristic helmet" -o helmet.png
```

### Extract from an existing image

```
npx tsx transparent-banana.ts -i photo.jpg "the vase" -o vase.png
```

### Skip generation with pre-made images

```
npx tsx transparent-banana.ts --white white.png --black black.png -o result.png
```

### Options

| Flag | Description |
|---|---|
| `-i, --input <path>` | Input image to extract an object from |
| `-o, --output <path>` | Output file path (default: `output.png`) |
| `-m, --model <name>` | Nano Banana model (default: `gemini-3-pro-image-preview`) |
| `--white <path>` | Pre-generated white-background image |
| `--black <path>` | Pre-generated black-background image |
| `--save-intermediates` | Save the white and black intermediate images |

## Models

We recommend **Nano Banana Pro** for best results.

| Model | ID | Description |
|---|---|---|
| **Nano Banana Pro** | `gemini-3-pro-image-preview` | Designed for professional asset production, utilizing advanced reasoning ("Thinking") to follow complex instructions and render high-fidelity text. |
| Nano Banana | `gemini-2.5-flash-image` | Designed for speed and efficiency, optimized for high-volume, low-latency tasks. |

## Setup

```bash
npm install
export GEMINI_API_KEY="your-key"
```

Requires a [Nano Banana API key](https://ai.google.dev/).
