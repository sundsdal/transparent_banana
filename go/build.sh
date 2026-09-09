#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output_dir="$script_dir/dist"
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/nanobanana-build.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT HUP INT TERM

cp "$script_dir"/*.go "$script_dir/go.mod" "$temp_dir"/

(
    cd "$script_dir"
    go run ./internal/buildkey "$temp_dir/zz_bundled_key.go" "$script_dir/.env" "$script_dir/../.env"
)

mkdir -p "$output_dir"
(
    cd "$temp_dir"
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$output_dir/nanobanana" .
)

printf 'Built: %s\n' "$output_dir/nanobanana"
