#!/usr/bin/env bash
# Fails when a module depends on a sibling module it is not allowed to (layering rules in docs/ARCHITECTURE.md).
set -euo pipefail
cd "$(dirname "$0")/.."
M=github.com/rlsvarinskis/cloud-at-home
declare -A ALLOWED=(
  [api]=""
  [libs/common]=""
  [libs/authz]="api libs/common"
  [libs/qemu]="api libs/common libs/authz"
  [libs/server]="api libs/common libs/authz"
  [services/vmd]="api libs/common libs/authz libs/qemu libs/server"
  [services/agentd]="api libs/common libs/authz libs/server"
  [services/mcp]="api libs/common libs/authz libs/server"
  [services/auth]="api libs/common libs/authz libs/server"
  [cmd/atticctl]="api libs/common libs/authz"
  [e2e]="api libs/common libs/authz libs/qemu libs/server services/vmd services/agentd services/mcp"
)
bad=0
for mod in "${!ALLOWED[@]}"; do
  [ -f "$mod/go.mod" ] || continue
  # sibling modules actually imported by Go files (tests included)
  used=$(grep -rhoE "\"$M/[a-z/]+" --include=*.go "$mod" | sed "s|\"$M/||" | awk -F/ '{ if ($1=="api") print "api"; else print $1"/"$2 }' | sort -u)
  for dep in $used; do
    [ "$dep" = "$mod" ] && continue
    case " ${ALLOWED[$mod]} " in *" $dep "*) ;; *) echo "FORBIDDEN: $mod imports $dep"; bad=1;; esac
  done
done
[ $bad = 0 ] && echo "module boundaries OK"
exit $bad
