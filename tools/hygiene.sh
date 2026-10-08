#!/bin/sh
# RHZ-099 / FR-RHZ-125: keep runtime state, credentials, and machine-local paths out.
set -eu

check_file() {
	path=$1
	case "$path" in
		*.ndjson|*.db|*.sqlite|*.sqlite3|*.db-*|*.ndjson.lock|*.log|*/.rhizome*|.rhizome*)
			echo "hygiene: runtime data file: $path" >&2
			printf '%s\n' "$path" >> "$violations"
			return 0
		;;
	esac
	[ -s "$path" ] || return 0
	if ! LC_ALL=C grep -qI . "$path" 2>/dev/null; then
		return 0
	fi
	case "$path" in
		*/package-lock.json|package-lock.json) content=$(sed '/"integrity"[[:space:]]*:/d' "$path") ;;
		*) content=$(cat "$path") ;;
	esac
	users_path='/Users/'
	home_path='/home/'
	ghpat='github'_'pat_'
	boundary='(^|[^A-Za-z0-9_-])'
	patterns='-----BEGIN [^-]*PRIVATE KEY-----'
	patterns=$patterns"|$boundary"'AKIA[0-9A-Z]{16}'
	patterns=$patterns"|$boundary"'gh[pousr]_[A-Za-z0-9]{30,}'
	patterns=$patterns"|$boundary"$ghpat
	patterns=$patterns"|$boundary"'sk-ant-[A-Za-z0-9_-]{20,}'
	patterns=$patterns"|$boundary"'sk-[A-Za-z0-9_-]{20,}'
	patterns=$patterns"|"'xox[abprs]-'
	patterns=$patterns"|"'AIza[0-9A-Za-z_-]{35}'
	patterns=$patterns"|"'(^|[^0-9.])100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}([^0-9]|$)'
	patterns=$patterns"|"$users_path'[^/[:space:]]+/|'''$home_path'[^/[:space:]]+/'
	if printf '%s' "$content" | grep -Eq -- "$patterns"; then
		echo "hygiene: sensitive or machine-local content: $path" >&2
		printf '%s\n' "$path" >> "$violations"
	fi
	return 0
}

# xargs invokes this same script once per NUL-delimited path. A shared marker
# file carries detections back to the parent because shell variables in a
# pipeline/subprocess do not propagate to the caller.
if [ "${1-}" = "--check-file" ]; then
	violations=${2-}
	path=${3-}
	[ -n "$violations" ] && [ -n "$path" ] || exit 0
	check_file "$path"
	exit 0
fi

script_path=$0
case "$script_path" in
	/*) ;;
	*) script_path=$(CDPATH= cd -- "$(dirname -- "$script_path")" && pwd)/$(basename -- "$script_path") ;;
esac
script_dir=$(CDPATH= cd -- "$(dirname -- "$script_path")" && pwd)
start=$(pwd)
if git -C "$start" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	root=$(git -C "$start" rev-parse --show-toplevel)
	cd "$root"
	files=$(mktemp "${TMPDIR:-/tmp}/rhizome-hygiene-files.XXXXXX")
	violations=$(mktemp "${TMPDIR:-/tmp}/rhizome-hygiene-violations.XXXXXX")
	trap 'rm -f "$files" "$violations"' EXIT HUP INT TERM
	git ls-files -z > "$files"
else
	# A copied tools/hygiene.sh has the same tools/.. layout as the repository.
	root=$(CDPATH= cd -- "$script_dir/.." && pwd)
	cd "$root"
	files=$(mktemp "${TMPDIR:-/tmp}/rhizome-hygiene-files.XXXXXX")
	violations=$(mktemp "${TMPDIR:-/tmp}/rhizome-hygiene-violations.XXXXXX")
	trap 'rm -f "$files" "$violations"' EXIT HUP INT TERM
	find . -type f -not -path './.git/*' -not -path '*/node_modules/*' -print0 > "$files"
fi

if ! xargs -0 -n 1 sh "$script_path" --check-file "$violations" < "$files"; then
	echo "hygiene: scanner failed" >&2
	exit 1
fi
[ ! -s "$violations" ]
