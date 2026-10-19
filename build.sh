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
temp_output=$(mktemp "$output_dir/.generate_media.XXXXXX")
cp "$script_dir/go/dist/generate_media" "$temp_output"
chmod 755 "$temp_output"
mv -f "$temp_output" "$output_dir/generate_media"
temp_output=

ln -sfn generate_media "$output_dir/generate"
ln -sfn generate_media "$output_dir/nanobanana"
printf 'Built: %s\n' "$output_dir/generate_media"
