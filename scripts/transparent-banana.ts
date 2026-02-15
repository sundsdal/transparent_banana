import { GoogleGenAI } from "@google/genai";
import sharp from "sharp";
import fs from "fs/promises";
import path from "path";

const DEFAULT_MODEL = "gemini-3-pro-image-preview";

async function generateOnWhite(
  ai: GoogleGenAI,
  prompt: string,
  model: string
): Promise<Buffer> {
  const textPrompt = `${prompt}. On a pure solid white #FFFFFF background`;
  console.log("Step 1/3: Generating image on white background...");
  console.log(`  Prompt: "${textPrompt}"`);

  const response = await ai.models.generateContent({
    model,
    contents: textPrompt,
    config: {
      responseModalities: ["IMAGE"],
    },
  });

  const parts = response.candidates?.[0]?.content?.parts;
  if (!parts) throw new Error("No response from model");

  for (const part of parts) {
    if (part.inlineData) {
      return Buffer.from(part.inlineData.data!, "base64");
    }
  }

  throw new Error("No image in response");
}

async function extractObjectOnWhite(
  ai: GoogleGenAI,
  inputImage: Buffer,
  prompt: string,
  model: string
): Promise<Buffer> {
  const textPrompt = `This image contains ${prompt}. Remove everything from the scene except ${prompt}. Place the isolated object on a plain, pure white #FFFFFF background with no shadows, no reflections, and no other elements. Do not change the object itself in any way — preserve its exact colors, shape, size, and details.`;
  console.log("Step 1/3: Extracting object onto white background...");
  console.log(`  Prompt: "${textPrompt}"`);

  const base64Image = inputImage.toString("base64");

  const response = await ai.models.generateContent({
    model,
    contents: [
      {
        text: textPrompt,
      },
      {
        inlineData: {
          mimeType: "image/png",
          data: base64Image,
        },
      },
    ],
    config: {
      responseModalities: ["IMAGE"],
    },
  });

  const parts = response.candidates?.[0]?.content?.parts;
  if (!parts) throw new Error("No response from model");

  for (const part of parts) {
    if (part.inlineData) {
      return Buffer.from(part.inlineData.data!, "base64");
    }
  }

  throw new Error("No image in response");
}

async function editToBlack(
  ai: GoogleGenAI,
  whiteImage: Buffer,
  model: string
): Promise<Buffer> {
  const textPrompt = "Change the white background to a solid pure black #000000 background. Keep everything else exactly unchanged.";
  console.log("Step 2/3: Editing background to black...");
  console.log(`  Prompt: "${textPrompt}"`);

  const base64Image = whiteImage.toString("base64");

  const response = await ai.models.generateContent({
    model,
    contents: [
      {
        text: textPrompt,
      },
      {
        inlineData: {
          mimeType: "image/png",
          data: base64Image,
        },
      },
    ],
    config: {
      responseModalities: ["IMAGE"],
    },
  });

  const parts = response.candidates?.[0]?.content?.parts;
  if (!parts) throw new Error("No response from edit model");

  for (const part of parts) {
    if (part.inlineData) {
      return Buffer.from(part.inlineData.data!, "base64");
    }
  }

  throw new Error("No image in edit response");
}

async function extractAlpha(
  whiteImageBuf: Buffer,
  blackImageBuf: Buffer,
  outputPath: string
): Promise<void> {
  console.log("Step 3/3: Extracting alpha via difference matting...");

  const { data: dataWhite, info: meta } = await sharp(whiteImageBuf)
    .ensureAlpha()
    .raw()
    .toBuffer({ resolveWithObject: true });

  const { data: dataBlack } = await sharp(blackImageBuf)
    .resize(meta.width, meta.height)
    .ensureAlpha()
    .raw()
    .toBuffer({ resolveWithObject: true });

  if (dataWhite.length !== dataBlack.length) {
    throw new Error(
      `Size mismatch after resize: white=${dataWhite.length} black=${dataBlack.length}`
    );
  }

  const outputBuffer = Buffer.alloc(dataWhite.length);
  const bgDist = Math.sqrt(3 * 255 * 255);

  for (let i = 0; i < meta.width * meta.height; i++) {
    const offset = i * 4;

    const rW = dataWhite[offset];
    const gW = dataWhite[offset + 1];
    const bW = dataWhite[offset + 2];

    const rB = dataBlack[offset];
    const gB = dataBlack[offset + 1];
    const bB = dataBlack[offset + 2];

    const pixelDist = Math.sqrt(
      (rW - rB) ** 2 + (gW - gB) ** 2 + (bW - bB) ** 2
    );

    let alpha = Math.max(0, Math.min(1, 1 - pixelDist / bgDist));

    let rOut = 0,
      gOut = 0,
      bOut = 0;

    if (alpha > 0.01) {
      rOut = rB / alpha;
      gOut = gB / alpha;
      bOut = bB / alpha;
    }

    outputBuffer[offset] = Math.round(Math.min(255, rOut));
    outputBuffer[offset + 1] = Math.round(Math.min(255, gOut));
    outputBuffer[offset + 2] = Math.round(Math.min(255, bOut));
    outputBuffer[offset + 3] = Math.round(alpha * 255);
  }

  await sharp(outputBuffer, {
    raw: { width: meta.width, height: meta.height, channels: 4 },
  })
    .png()
    .toFile(outputPath);
}

function printUsage(): void {
  console.log(`
transparent-banana - Generate transparent PNGs using Gemini + difference matting

Usage:
  npx tsx transparent-banana.ts <prompt> [options]
  npx tsx transparent-banana.ts -i <image> <prompt> [options]
  npx tsx transparent-banana.ts --white <white.png> --black <black.png> [options]

Options:
  -i, --input <path>      Input image to extract an object from
  -o, --output <path>     Output file path (default: output.png)
  -m, --model <name>      Gemini model (default: ${DEFAULT_MODEL})
  --white <path>          Path to pre-generated white-background image (skip generation)
  --black <path>          Path to pre-generated black-background image (skip generation & edit)
  --save-intermediates    Save the white and black intermediate images

Environment:
  GEMINI_API_KEY          Your Google Gemini API key (required for generation)

Examples:
  npx tsx transparent-banana.ts "a futuristic helmet with shadow"
  npx tsx transparent-banana.ts -i photo.jpg "the vase" -o vase.png
  npx tsx transparent-banana.ts "a glass vase with flowers" -o vase.png --save-intermediates
  npx tsx transparent-banana.ts --white helmet-white.png --black helmet-black.png -o helmet.png
`);
}

function parseArgs(argv: string[]): {
  prompt: string | null;
  inputPath: string | null;
  output: string;
  model: string;
  whitePath: string | null;
  blackPath: string | null;
  saveIntermediates: boolean;
} {
  const args = argv.slice(2);
  let prompt: string | null = null;
  let inputPath: string | null = null;
  let output = "output.png";
  let model = DEFAULT_MODEL;
  let whitePath: string | null = null;
  let blackPath: string | null = null;
  let saveIntermediates = false;

  for (let i = 0; i < args.length; i++) {
    switch (args[i]) {
      case "-h":
      case "--help":
        printUsage();
        process.exit(0);
      case "-i":
      case "--input":
        inputPath = args[++i];
        break;
      case "-o":
      case "--output":
        output = args[++i];
        break;
      case "-m":
      case "--model":
        model = args[++i];
        break;
      case "--white":
        whitePath = args[++i];
        break;
      case "--black":
        blackPath = args[++i];
        break;
      case "--save-intermediates":
        saveIntermediates = true;
        break;
      default:
        if (!args[i].startsWith("-")) {
          prompt = args[i];
        }
        break;
    }
  }

  return { prompt, inputPath, output, model, whitePath, blackPath, saveIntermediates };
}

async function main(): Promise<void> {
  const config = parseArgs(process.argv);

  if (!config.prompt && !config.whitePath) {
    printUsage();
    process.exit(1);
  }

  if (config.inputPath && !config.prompt) {
    console.error("Error: A prompt describing which object to extract is required with -i");
    process.exit(1);
  }

  let whiteImage: Buffer;
  let blackImage: Buffer;

  if (config.whitePath && config.blackPath) {
    // Both images provided — just extract alpha
    console.log("Using provided white and black images...");
    whiteImage = await fs.readFile(config.whitePath);
    blackImage = await fs.readFile(config.blackPath);
  } else {
    // Need API key for generation
    const apiKey = process.env.GEMINI_API_KEY;
    if (!apiKey) {
      console.error("Error: GEMINI_API_KEY environment variable is required");
      process.exit(1);
    }

    const ai = new GoogleGenAI({ apiKey });

    if (config.whitePath) {
      // White image provided, only do the edit step
      whiteImage = await fs.readFile(config.whitePath);
      blackImage = await editToBlack(ai, whiteImage, config.model);
    } else if (config.inputPath) {
      // Extract object from input image onto white, then edit to black
      const inputImage = await fs.readFile(config.inputPath);
      whiteImage = await extractObjectOnWhite(ai, inputImage, config.prompt!, config.model);
      blackImage = await editToBlack(ai, whiteImage, config.model);
    } else {
      // Full pipeline: generate on white, edit to black
      whiteImage = await generateOnWhite(ai, config.prompt!, config.model);
      blackImage = await editToBlack(ai, whiteImage, config.model);
    }

    if (config.saveIntermediates) {
      const dir = path.dirname(config.output);
      const base = path.basename(config.output, path.extname(config.output));
      const whitePath = path.join(dir, `${base}-white.png`);
      const blackPath = path.join(dir, `${base}-black.png`);

      await sharp(whiteImage).png().toFile(whitePath);
      await sharp(blackImage).png().toFile(blackPath);
      console.log(`Saved intermediates: ${whitePath}, ${blackPath}`);
    }
  }

  await extractAlpha(whiteImage, blackImage, config.output);
  console.log(`Done! Transparent PNG saved to: ${config.output}`);
}

main().catch((err) => {
  console.error("Error:", err.message);
  process.exit(1);
});
