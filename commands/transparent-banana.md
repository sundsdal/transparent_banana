---
description: Generate a transparent PNG using Gemini and difference matting
allowed-tools: Bash(*), Read, Glob, AskUserQuestion
---

You are helping the user generate a transparent PNG image using the transparent-banana tool. This tool uses Gemini image generation and a difference matting technique to produce clean transparency.

## How it works

1. Generates (or extracts) an object on a **white** background
2. Edits that image to have a **black** background
3. Computes the alpha channel mathematically from the difference between the two

## Setup check

First, check if dependencies are installed:

```
!`test -d ${CLAUDE_PLUGIN_ROOT}/scripts/node_modules && echo "DEPS_INSTALLED" || echo "DEPS_MISSING"`
```

If DEPS_MISSING, install them:
```bash
cd ${CLAUDE_PLUGIN_ROOT}/scripts && npm install
```

Then check for the API key (needed for generate and extract modes):
```
!`test -n "$GEMINI_API_KEY" && echo "KEY_SET" || echo "KEY_MISSING"`
```

## Determine mode

There are three modes. If the user provided arguments via `$ARGUMENTS`, use those to determine the mode. Otherwise, ask the user using AskUserQuestion:

**Mode 1 - Generate**: Create a new image from a text prompt. Requires GEMINI_API_KEY.
**Mode 2 - Extract**: Isolate an object from an existing image. Requires GEMINI_API_KEY and an input image path.
**Mode 3 - Alpha only**: Combine pre-existing white-background and black-background images into a transparent PNG. No API key needed.

## Gather parameters

Based on the mode, gather the needed information conversationally:

### For Generate mode:
- **Prompt** (required): What to generate (e.g., "a futuristic helmet", "a glass vase with flowers")
- **Output path** (optional, default: output.png): Where to save the result
- **Model** (optional, default: gemini-3-pro-image-preview): Which Gemini model to use
- **Save intermediates** (optional, default: no): Whether to save the white/black intermediate images

### For Extract mode:
- **Input image path** (required): Path to the source image
- **Prompt** (required): What object to extract (e.g., "the vase", "the person on the left")
- **Output path** (optional, default: output.png)
- **Model** (optional, default: gemini-3-pro-image-preview)
- **Save intermediates** (optional, default: no)

### For Alpha only mode:
- **White image path** (required): Path to white-background image
- **Black image path** (required): Path to black-background image
- **Output path** (optional, default: output.png)

## Execute

Build and run the command. The script is at `${CLAUDE_PLUGIN_ROOT}/scripts/transparent-banana.ts`.

Example commands:

```bash
# Generate mode
cd ${CLAUDE_PLUGIN_ROOT}/scripts && npx tsx transparent-banana.ts "a futuristic helmet" -o /path/to/output.png

# Generate with intermediates saved
cd ${CLAUDE_PLUGIN_ROOT}/scripts && npx tsx transparent-banana.ts "a glass vase" -o /path/to/output.png --save-intermediates

# Extract mode
cd ${CLAUDE_PLUGIN_ROOT}/scripts && npx tsx transparent-banana.ts -i /path/to/photo.jpg "the vase" -o /path/to/output.png

# Alpha only mode
cd ${CLAUDE_PLUGIN_ROOT}/scripts && npx tsx transparent-banana.ts --white /path/to/white.png --black /path/to/black.png -o /path/to/output.png
```

Important:
- Always use absolute paths for input/output files (resolve relative to the user's working directory)
- The script must be run from `${CLAUDE_PLUGIN_ROOT}/scripts/` so it finds its node_modules
- If GEMINI_API_KEY is missing and the user needs generate/extract mode, tell them to set it: `export GEMINI_API_KEY="your-key"`
- The process can take 30-60 seconds per Gemini API call (there are 2 calls for generate/extract mode)
- Report the output file path when done

## Available models

| Model | ID | Notes |
|---|---|---|
| **Gemini 3 Pro** (default) | `gemini-3-pro-image-preview` | Best quality, uses advanced reasoning |
| Gemini 2.5 Flash | `gemini-2.5-flash-image` | Faster, good for high-volume tasks |
