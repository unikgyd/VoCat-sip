#!/bin/sh
# Shared Linux release build. Native armv7 targets Debian/Ubuntu armhf, not the
# linker's historic soft-float loader path. No host loader symlink is required.
set -eu
if [ "$#" -ne 2 ]; then
  printf '%s\n' 'usage: sh scripts/build-linux.sh OUTPUT native|static' >&2
  exit 2
fi
output=$1
flavor=$2
arch=${GOARCH:-$(go env GOARCH)}
case "$arch" in amd64|386|arm64|arm) ;; *) printf '%s\n' 'unsupported Linux release architecture' >&2; exit 2 ;; esac
flags="-s -w -X vocat/internal/buildinfo.Version=${VERSION:-0.1.0-dev} -X vocat/internal/buildinfo.BuildTime=${BUILD_TIME:-}"
case "$flavor" in
  native)
    if [ "$arch" = arm ]; then
      if [ "${GOARM:-7}" != 7 ]; then printf '%s\n' 'native arm release requires GOARM=7 (armhf)' >&2; exit 2; fi
      flags="$flags -I /lib/ld-linux-armhf.so.3"
    fi
    ;;
  static) ;;
  *) printf '%s\n' 'unknown release flavor' >&2; exit 2 ;;
esac
if [ "$arch" = arm ]; then GOARM=${GOARM:-7}; export GOARM; fi
mkdir -p "$(dirname "$output")"
if [ "$flavor" = static ]; then
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -tags=vocat_static -ldflags "$flags" -o "$output" ./cmd/vocat
else
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$flags" -o "$output" ./cmd/vocat
fi
