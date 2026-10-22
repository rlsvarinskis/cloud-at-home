#!/usr/bin/env bash
# Fails when a module depends on a sibling module it is not allowed to (layering rules in CONTRIBUTING.md#layering-rules).
#
# A module is a directory with a go.mod that appears in ALLOWED. Modules can nest (services/vmd/cli lives inside
# services/vmd), so every file and every import is attributed to the module with the longest matching directory:
# the file belongs to its own module, not to the one around it. Rows may contain shell globs (services/*/cli).
set -euo pipefail
cd "$(dirname "$0")/.."
M=github.com/rlsvarinskis/cloud-at-home
declare -A ALLOWED=(
  [api]=""
  [libs/common]=""
  [libs/authz]="api libs/common"
  [libs/qemu]="api libs/common libs/authz"
  [libs/server]="api libs/common libs/authz"
  [libs/cli]="api libs/common"
  [services/vmd]="api libs/common libs/authz libs/qemu libs/server"
  [services/vmd/cli]="api libs/common libs/cli"
  [services/agentd]="api libs/common libs/authz libs/server"
  [services/mcp]="api libs/common libs/authz libs/server"
  # Reserved for the planned auth service (docs/design/auth-graph.md). No such module exists yet; the
  # loop below skips rows whose directory has no go.mod, so this rule is not enforced until it does.
  [services/auth]="api libs/common libs/authz libs/server"
  [cmd/atticctl]="api libs/common libs/authz"
  [cmd/attic]="libs/cli services/*/cli"
  [e2e]="api libs/common libs/authz libs/qemu libs/server services/vmd services/agentd services/mcp"
)

# Every module on disk must have a row, or its files would be attributed to the module around it.
bad=0
while IFS= read -r gm; do
  d=${gm#./}
  d=${d%/go.mod}
  if [ -z "${ALLOWED[$d]+x}" ]; then
    echo "UNLISTED: module $d has no row in scripts/check-boundaries.sh"
    bad=1
  fi
done < <(find . -name go.mod -not -path './.git/*' -not -path './.claude/*')

# The modules that exist: rows whose directory has a go.mod.
mods=()
for mod in "${!ALLOWED[@]}"; do
  [ -f "$mod/go.mod" ] && mods+=("$mod")
done

# owner PATH prints the existing module whose directory is the longest prefix of PATH (a file path, or an import
# path with $M/ removed). It prints nothing when no module contains PATH.
owner() {
  local p=$1 best="" m
  for m in "${mods[@]}"; do
    if [ "$p" = "$m" ] || [[ "$p" == "$m/"* ]]; then
      if [ ${#m} -gt ${#best} ]; then
        best=$m
      fi
    fi
  done
  printf '%s' "$best"
}

# allowed FROM TO succeeds when a row of FROM matches TO (globs allowed).
allowed() {
  local from=$1 to=$2 entry
  local -a entries
  read -ra entries <<< "${ALLOWED[$from]}"
  for entry in "${entries[@]+"${entries[@]}"}"; do
    # shellcheck disable=SC2053 # the row entry is a glob on purpose
    if [[ "$to" == $entry ]]; then
      return 0
    fi
  done
  return 1
}

while IFS= read -r file; do
  from=$(owner "${file#./}")
  [ -n "$from" ] || continue
  while IFS= read -r imp; do
    to=$(owner "${imp#"$M/"}")
    [ -n "$to" ] || continue
    [ "$to" = "$from" ] && continue
    if ! allowed "$from" "$to"; then
      echo "FORBIDDEN: $from imports $to (in ${file#./})"
      bad=1
    fi
  done < <(grep -hoE "\"$M/[^\"]+\"" "$file" | tr -d '"')
done < <(find . -name '*.go' -not -path './.git/*' -not -path './.claude/*')

[ $bad = 0 ] && echo "module boundaries OK"
exit $bad
