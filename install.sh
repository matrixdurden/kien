#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "kien supports Linux only." >&2
  exit 1
fi

if ! command -v go >/dev/null; then
  echo "Go 1.22 or newer is required to build kien." >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin_dir="${XDG_BIN_HOME:-$HOME/.local/bin}"
destination="$bin_dir/kien"
temporary="$destination.tmp"

mkdir -p "$bin_dir"
go build -o "$temporary" "$root"
chmod 755 "$temporary"
mv -f "$temporary" "$destination"

echo "Installed kien to $destination"
case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo "Add $bin_dir to PATH to run kien from any directory." ;;
esac
