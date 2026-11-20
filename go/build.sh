#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output_dir="$script_dir/dist"
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/nanobanana-build.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT HUP INT TERM

cp "$script_dir"/*.go "$script_dir/go.mod" "$script_dir/go.sum" "$temp_dir"/
version=$(git -C "$script_dir" describe --tags --always --dirty 2>/dev/null || echo dev)

(
    cd "$script_dir"
    go run ./internal/buildkey "$temp_dir/zz_bundled_key.go" "$script_dir/.env" "$script_dir/../.env"
)

mkdir -p "$output_dir"
(
    cd "$temp_dir"
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$output_dir/generate_media" .
)

ln -sfn generate_media "$output_dir/generate"
ln -sfn generate_media "$output_dir/nanobanana"
printf 'Built: %s\n' "$output_dir/generate_media"
