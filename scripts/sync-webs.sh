#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
source_zip=${1:-"$project_dir/../nav-oss-lib-web/dist/nav-oss-lib-web.zip"}
target_zip="$project_dir/webs/assets/nav-oss-lib-web.zip"

if [ ! -f "$source_zip" ]; then
	printf 'web archive not found: %s\n' "$source_zip" >&2
	exit 1
fi

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/nav-oss-webs.XXXXXX")
cleanup() {
	rm -rf -- "$temp_dir"
}
trap cleanup EXIT HUP INT TERM

unzip -q "$source_zip" -d "$temp_dir"

bundle_dir="$temp_dir/nav-oss-lib-web"
if [ ! -f "$bundle_dir/browser/index.html" ]; then
	printf 'invalid web archive: nav-oss-lib-web/browser/index.html is missing\n' >&2
	exit 1
fi

set -- nav-oss-lib-web/browser
if [ -f "$bundle_dir/3rdpartylicenses.txt" ]; then
	set -- "$@" nav-oss-lib-web/3rdpartylicenses.txt
fi
if [ -f "$bundle_dir/prerendered-routes.json" ]; then
	set -- "$@" nav-oss-lib-web/prerendered-routes.json
fi

normalized_zip="$temp_dir/nav-oss-lib-web.zip"
(
	cd "$temp_dir"
	zip -X -q -r "$normalized_zip" "$@"
)
unzip -tq "$normalized_zip" >/dev/null
mv "$normalized_zip" "$target_zip"

printf 'synced web archive: %s\n' "$target_zip"
