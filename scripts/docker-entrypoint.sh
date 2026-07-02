#!/bin/sh
set -e

: "${BKCRAB_HOME:=/data/.bkcrab}"

seed_plugins() {
  src_root="/usr/local/share/bkcrab/plugins"
  dst_root="$BKCRAB_HOME/plugins"

  mkdir -p "$dst_root"
  [ -d "$src_root" ] || return 0

  for src in "$src_root"/*; do
    [ -d "$src" ] || continue
    name="$(basename "$src")"
    dst="$dst_root/$name"
    if [ ! -e "$dst" ]; then
      cp -R "$src" "$dst"
    fi
  done
}

seed_plugins

exec "$@"
