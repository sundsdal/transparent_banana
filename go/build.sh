#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output_dir="$script_dir/dist"
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/nanobanana-build.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT HUP INT TERM

cp "$script_dir"/*.go "$script_dir/go.mod" "$temp_dir"/

if [ -n "${NANOBANANA_BUNDLE_KEY:-}" ]; then
    {
        printf '%s\n' 'package main' '' 'import "encoding/base64"' '' 'func init() {'
        printf '%s' '    decoded, err := base64.StdEncoding.DecodeString("'
        printf %s "$NANOBANANA_BUNDLE_KEY" | base64 | tr -d '\n'
        printf '%s\n' '")' '    if err != nil {' '        panic(err)' '    }' '    bundledAPIKey = string(decoded)' '}'
    } > "$temp_dir/zz_bundled_key.go"
fi

mkdir -p "$output_dir"
(
    cd "$temp_dir"
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$output_dir/nanobanana" .
)

printf 'Built: %s\n' "$output_dir/nanobanana"
