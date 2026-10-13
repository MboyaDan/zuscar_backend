#!/usr/bin/env bash
# SANDBOX ONLY. Resolves Go modules when proxy.golang.org and vanity domains (golang.org, gopkg.in,
# go.uber.org, google.golang.org, go.mongodb.org) are unreachable but github.com is. For each module
# that fails to resolve it builds the module zip from its GitHub mirror with the same zip algorithm the
# real proxy uses, into a local file proxy, then re-runs `go mod tidy`. The result in go.mod is clean (no
# replace lines). go.sum hashes are computed locally and are NOT checked against sum.golang.org.
# On a normal machine run `cd backend && go mod tidy && go mod verify` and confirm go.mod/go.sum do not change.
# One-time build of the generator (needs a clone of github.com/golang/mod):
#   git clone --depth 1 https://github.com/golang/mod /tmp/xmod && mkdir /tmp/xmod/gen \
#     && cp scripts/sandbox/xproxy-gen.go /tmp/xmod/gen/main.go && (cd /tmp/xmod && go build -o /usr/local/bin/xproxy-gen ./gen)
set -uo pipefail
export GOSUMDB=off GOFLAGS=-mod=mod GOPROXY="file:///tmp/gomodproxy,direct" GONOSUMDB="*" GOTOOLCHAIN=local
cd "$(dirname "$0")/../.."
for i in $(seq 1 80); do
  out=$(go mod tidy 2>&1) && { echo "tidy OK after $i runs"; exit 0; }
  pair=$(echo "$out" | grep -oP '[^\s]+@v[^\s:]+(?=: unrecognized import path)' | head -1)
  if [ -z "$pair" ]; then
    pair=$(echo "$out" | grep -oP '(?<=downloading )\S+ v\S+' | while read m v; do
      case "$m" in golang.org/x/*|gopkg.in/*|go.uber.org/*|google.golang.org/*|go.mongodb.org/*)
        [ -f "/tmp/gomodproxy/$m/@v/$v.zip" ] || { echo "$m@$v"; break; };; esac; done)
  fi
  [ -n "$pair" ] || { echo "UNHANDLED:"; echo "$out" | tail -6; exit 1; }
  xproxy-gen "${pair%@*}" "${pair#*@}" /tmp/gomodproxy >/dev/null || exit 1
  echo "generated $pair"
done
