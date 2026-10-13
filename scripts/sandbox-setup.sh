#!/usr/bin/env bash
# Rebuilds the authoring sandbox toolchain (not needed on a normal developer machine, which has
# internet access to the Go module proxy). Verified manually, in pieces, during Stage 4.
# Why: the sandbox blocks proxy.golang.org, go.dev and golang.org, so Go is built from GitHub source
# and modules are resolved through a local file proxy built from GitHub mirrors (ADR-11).
set -euo pipefail
GO_TAG="${GO_TAG:-go1.27.2}"
apt-get update -qq
apt-get install -y -qq golang-1.24-go postgresql-16 postgresql-contrib   # 1.24 is the bootstrap compiler
if [ ! -x /usr/local/zuscar-go/bin/go ]; then
  git clone -q --depth 1 --branch "$GO_TAG" https://github.com/golang/go /usr/local/zuscar-go
  (cd /usr/local/zuscar-go/src && GOROOT_BOOTSTRAP=/usr/lib/go-1.24 GOTOOLCHAIN=local GOPROXY=off bash make.bash)
fi
curl -sL https://github.com/sqlc-dev/sqlc/releases/download/v1.31.1/sqlc_1.31.1_linux_amd64.tar.gz | tar xz -C /usr/local/bin sqlc
echo 'export PATH=/usr/local/zuscar-go/bin:/usr/lib/postgresql/16/bin:$PATH GOTOOLCHAIN=local' > /etc/profile.d/zuscar.sh
echo "done: source /etc/profile.d/zuscar.sh, build xproxy-gen and run scripts/sandbox/resolve-modules.sh (see its header)"
