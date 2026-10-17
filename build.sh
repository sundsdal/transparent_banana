#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output_dir="$script_dir/dist"
temp_output=

cleanup() {
    if [ -n "$temp_output" ]; then
        rm -f "$temp_output"
    fi
}

trap cleanup EXIT HUP INT TERM

"$script_dir/go/build.sh"

mkdir -p "$output_dir"
temp_output=$(mktemp "$output_dir/.generate.XXXXXX")
cp "$script_dir/go/dist/generate" "$temp_output"
chmod 755 "$temp_output"
mv -f "$temp_output" "$output_dir/generate"
temp_output=

ln -sfn generate "$output_dir/nanobanana"
printf 'Built: %s\n' "$output_dir/generate"
