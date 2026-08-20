#!/usr/bin/env bash
#
# check-banned-words.sh — fail the build if a retired internal codename leaks
# into source. A legacy codename ("Cere"+"bro") once reached customer-facing UI
# copy; this guard makes that class of regression impossible to merge.
#
# The banned tokens are assembled from fragments so this script never matches
# itself, and the scan covers this file too — no self-exclusion needed. Keep it
# fast (ripgrep if present, else git grep) and deterministic.
#
# Usage: scripts/check-banned-words.sh
# Exit 0 = clean, exit 1 = a banned word was found (with file:line printed).

set -euo pipefail

cd "$(dirname "$0")/.."

# Banned tokens, built from fragments so the literals never appear verbatim in
# this file. Case-insensitive match. Add more rows as codenames are retired.
BANNED=(
  "Cere""bro"
)

# Source scopes to scan. Only tracked, hand-written source — build output,
# deps, coverage reports and vendored/generated trees are excluded below.
SCOPES=(ui/src internal cmd pkg docs)

EXISTING_SCOPES=()
for scope in "${SCOPES[@]}"; do
  [ -e "$scope" ] && EXISTING_SCOPES+=("$scope")
done

# Directories/globs never worth scanning (generated or vendored).
EXCLUDES=(node_modules dist build coverage vendor .git)

status=0
for token in "${BANNED[@]}"; do
  if command -v rg >/dev/null 2>&1; then
    glob_args=()
    for ex in "${EXCLUDES[@]}"; do
      glob_args+=(--glob "!**/$ex/**")
    done
    if rg --no-heading --line-number --ignore-case "${glob_args[@]}" \
        -- "$token" "${EXISTING_SCOPES[@]}"; then
      status=1
    fi
  else
    # Fallback: git grep (respects the index, ignores untracked build output).
    pathspecs=()
    for scope in "${EXISTING_SCOPES[@]}"; do
      pathspecs+=("$scope")
    done
    for ex in "${EXCLUDES[@]}"; do
      pathspecs+=(":(exclude)**/$ex/**")
    done
    if git grep -n -I --ignore-case -e "$token" -- "${pathspecs[@]}"; then
      status=1
    fi
  fi
done

if [ "$status" -ne 0 ]; then
  echo ""
  echo "ERROR: a retired internal codename was found in source (see above)."
  echo "Customer-facing and internal source must say \"Squadron\". Replace it and re-run."
  exit 1
fi

echo "banned-words: clean (no retired codenames in source)."
