#!/bin/bash
set -e

# Set NANOBANANA_BUNDLE_KEY to bake an API key into the binary.
# At runtime, GEMINI_API_KEY env var takes priority over the bundled key.
if [ -z "$NANOBANANA_BUNDLE_KEY" ]; then
    echo "Note: No NANOBANANA_BUNDLE_KEY set. Binary will require GEMINI_API_KEY at runtime."
fi

uv venv .venv --python 3.12 2>/dev/null || true
uv pip install --python .venv google-genai Pillow numpy pyinstaller

# Inject bundled key into a temp copy for building
cp nanobanana nanobanana.build.py
if [ -n "$NANOBANANA_BUNDLE_KEY" ]; then
    sed -i '' "s|BUNDLED_API_KEY = os.environ.get(\"NANOBANANA_BUNDLE_KEY\", \"\")|BUNDLED_API_KEY = \"$NANOBANANA_BUNDLE_KEY\"|" nanobanana.build.py
fi

rm -rf build dist *.spec
.venv/bin/pyinstaller --onefile --name nanobanana --clean nanobanana.build.py
rm -rf build *.spec nanobanana.build.py
echo "Built: dist/nanobanana ($(du -h dist/nanobanana | cut -f1))"
