#!/usr/bin/env bash
set -euo pipefail

bin_dir="${XDG_BIN_HOME:-$HOME/.local/bin}"
binary="$bin_dir/kien"

if [[ -x "$binary" ]]; then
  while IFS= read -r session; do
    [[ -n "$session" ]] && "$binary" kill "$session"
  done < <("$binary" list)
fi

rm -f "$binary"
echo "Removed kien from $binary"
