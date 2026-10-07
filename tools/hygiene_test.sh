#!/bin/sh
# RHZ-099 / FR-RHZ-125: planted-file regression for the hygiene checker.
set -eu

here=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rhizome-hygiene.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir "$tmp/tools"
cp "$here/hygiene.sh" "$tmp/tools/hygiene.sh"
chmod +x "$tmp/tools/hygiene.sh"

clean="$tmp/clean"
mkdir -p "$clean/tools"
cp "$here/hygiene.sh" "$clean/tools/hygiene.sh"
chmod +x "$clean/tools/hygiene.sh"
: > "$clean/empty.txt"
printf '\000\377' > "$clean/binary.bin"
printf '%s\n' 'disk-usage-monitoring-toolkit' 'version 2100.70.1.2' > "$clean/path with space.txt"
if ! (cd "$clean" && sh tools/hygiene.sh) >/dev/null 2>&1; then
	echo "hygiene test: clean tree was rejected" >&2
	exit 1
fi

{
	printf 'AKIA%s\n' '1234567890ABCDEF'
	printf '%s.%s.%s.%s\n' 100 64 0 1
} > "$clean/bad.txt"
if output=$(cd "$clean" && sh tools/hygiene.sh 2>&1); then
	echo "hygiene test: planted credential was accepted" >&2
	exit 1
elif ! printf '%s\n' "$output" | grep -q 'bad\.txt'; then
	echo "hygiene test: planted credential failure did not name bad.txt" >&2
	exit 1
fi
