#!/bin/sh

set -eu

public_site_title="${PUBLIC_SITE_TITLE:-}"
case "$public_site_title" in
  *[![:space:]]*) ;;
  *)
    echo "PUBLIC_SITE_TITLE is required" >&2
    exit 1
    ;;
esac

runtime_directory=/usr/share/nginx/html/runtime
temporary_file="$runtime_directory/public-site-title.txt.tmp"
mkdir -p "$runtime_directory"
printf '%s' "$public_site_title" >"$temporary_file"
mv "$temporary_file" "$runtime_directory/public-site-title.txt"
