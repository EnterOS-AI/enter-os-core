#!/usr/bin/env bash
# reserved-path-match — shared matcher for the reserved-path self-merge guard.
#
# Sourced by BOTH layers so they cannot drift:
#   - reserved-path-review.sh (preventive CI gate)
#   - audit-force-merge.sh    (detective post-merge audit)
#
# Defines `reserved_paths_match_any <patterns_file> <changed_files...>`:
#   returns 0 (match) and prints the matched "FILE<TAB>PATTERN" pairs to stdout
#   if ANY changed file matches ANY pattern; returns 1 (no match) otherwise.
#
# Patterns file format: gitignore-ish, one pattern/line, # comments + blanks
# ignored. Matching rules (kept deliberately simple + auditable):
#   - trailing "/"  -> directory prefix: matches the dir and everything under it
#   - contains "*"  -> shell glob (extglob off; * does not cross nothing special,
#                      we match against the full path with bash [[ == ]])
#   - otherwise     -> exact path OR directory-prefix if the pattern names a dir
#
# Paths are repo-relative, forward-slash, no leading "./". Callers normalize.

set -euo pipefail

# Load patterns into a global array, skipping comments/blanks.
_rp_load_patterns() {
  local file="$1"
  RP_PATTERNS=()
  if [ ! -f "$file" ]; then
    echo "::error::reserved-paths file not found: $file" >&2
    return 2
  fi
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    # strip trailing CR (CRLF safety) and surrounding whitespace
    line="${line%$'\r'}"
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [ -z "$line" ] && continue
    case "$line" in \#*) continue ;; esac
    RP_PATTERNS+=("$line")
  done < "$file"
  if [ "${#RP_PATTERNS[@]}" -eq 0 ]; then
    echo "::error::reserved-paths file has zero usable patterns: $file" >&2
    return 2
  fi
  return 0
}

# Does a single normalized path match a single pattern?
_rp_one() {
  local path="$1" pat="$2"
  case "$pat" in
    */)
      # directory prefix
      [[ "$path" == "$pat"* ]] && return 0 ;;
    *'*'*)
      # glob anywhere
      # shellcheck disable=SC2053
      [[ "$path" == $pat ]] && return 0 ;;
    *)
      # exact, OR treat as dir-prefix when pattern itself is a dir-like prefix
      [[ "$path" == "$pat" ]] && return 0
      [[ "$path" == "$pat"/* ]] && return 0 ;;
  esac
  return 1
}

# reserved_paths_match_any <patterns_file> <changed_file>...
# stdout: matched "FILE<TAB>PATTERN" lines. return 0 if any matched.
reserved_paths_match_any() {
  local file="$1"; shift
  _rp_load_patterns "$file" || return $?
  local matched=1 f pat
  for f in "$@"; do
    [ -z "$f" ] && continue
    f="${f#./}"
    for pat in "${RP_PATTERNS[@]}"; do
      if _rp_one "$f" "$pat"; then
        printf '%s\t%s\n' "$f" "$pat"
        matched=0
        break
      fi
    done
  done
  return $matched
}
