#!/usr/bin/env bash
#
# Build the pestilence control-plane image and load it into the k0s node's containerd.
#
# Pipeline: cross-compile (Go) -> buildah (WSL) -> docker-archive -> scp ->
# k0s ctr images import. The same pipeline scarab uses for its two images, so there
# is one way to get an image onto this node rather than two.
#
# The registry is deferred, so the image is imported locally and the Deployment
# references the same tag with imagePullPolicy: IfNotPresent.
#
# Requirements
#   - buildah inside WSL. Rootless is fine.
#   - an ssh alias for the node that works non-interactively.
#   - NOPASSWD for the k0s ctr image commands, because the k0s containerd socket is
#     root-only (srw-rw---- root:root). Add to /etc/sudoers.d/k0s-ctr-images:
#
#       $USER ALL=(root) NOPASSWD: /usr/local/bin/k0s ctr images import *, \
#                                   /usr/local/bin/k0s ctr images ls, \
#                                   /usr/local/bin/k0s ctr images rm *
#
# Usage
#   cluster-setup-scripts/load-image.sh                 # build + load
#   TAG=v2 cluster-setup-scripts/load-image.sh
#   SKIP_BUILD=1 cluster-setup-scripts/load-image.sh    # reuse an existing archive

set -euo pipefail

TAG="${TAG:-dev}"
NODE="${NODE:-k0s-node}"
NAME="pestilence-control-plane"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# git-bash paths (/c/Users/...) are /mnt/c/Users/... inside WSL.
WSL_ROOT="$(printf '%s' "$REPO_ROOT" | sed 's|^/\([a-zA-Z]\)/|/mnt/\1/|')"

GO_BIN="${GO:-}"
if [ -z "$GO_BIN" ]; then
	if [ -x "$HOME/.local/go/bin/go" ]; then GO_BIN="$HOME/.local/go/bin/go"; else GO_BIN=go; fi
fi

mkdir -p "$REPO_ROOT/image/dist"

if [ "${SKIP_BUILD:-0}" != "1" ]; then
	echo "==> cross-compile linux/amd64 control-plane"
	# CGO_ENABLED=0 is required, not merely preferred: the target is distroless
	# static, which has no libc, and the SQLite driver is pure Go so nothing needs
	# cgo in the first place.
	(cd "$REPO_ROOT" &&
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -trimpath \
			-o "image/control-plane/$NAME" ./cmd/control-plane)
fi

ARCHIVE="$REPO_ROOT/image/dist/$NAME-$TAG.tar"
WSL_ARCHIVE="$WSL_ROOT/image/dist/$NAME-$TAG.tar"

if [ "${SKIP_BUILD:-0}" != "1" ]; then
	echo "==> buildah bud $NAME:$TAG"
	wsl.exe -e bash -lc "set -e
		cd '$WSL_ROOT/image/control-plane'
		buildah bud -t '$NAME:$TAG' -f Dockerfile .
		rm -f '$WSL_ARCHIVE'
		buildah push '$NAME:$TAG' docker-archive:'$WSL_ARCHIVE':'$NAME:$TAG'"
fi

echo "==> scp archive to $NODE:/tmp/"
# scp runs on the host rather than inside the build environment: the build
# environment has no ssh key, and copying a private key into it is unnecessary.
scp -o BatchMode=yes "$ARCHIVE" "$NODE:/tmp/$NAME-$TAG.tar"

echo "==> k0s ctr images import $NAME:$TAG"
ssh -o BatchMode=yes "$NODE" "sudo -n k0s ctr images import /tmp/$NAME-$TAG.tar"

echo "==> images on $NODE"
ssh -o BatchMode=yes "$NODE" "sudo -n k0s ctr images ls | grep pestilence || true"

echo "==> loaded. Reference 'localhost/$NAME:$TAG' with imagePullPolicy: IfNotPresent."
# The tag does not change, so a running pod keeps the image it started with.
echo "==> NOTE: the running control plane keeps the OLD image until restarted:"
echo "         kubectl -n pestilence rollout restart deploy/control-plane"
