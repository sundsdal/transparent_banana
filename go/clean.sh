#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
rm -rf "$script_dir/dist"
printf 'Cleaned: %s\n' "$script_dir/dist"
