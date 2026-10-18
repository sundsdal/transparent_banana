#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
rm -rf "$script_dir/dist" "$script_dir/go/dist"
printf 'Cleaned build outputs.\n'
