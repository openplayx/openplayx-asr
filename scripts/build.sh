#!/usr/bin/env bash
# Builds the native service for the host: pinned whisper.cpp + the opx_asr
# C ABI from whisper-package, linked statically through cgo.
#   OPX_WHISPER_PACKAGE  path to whisper-package (default: sibling checkout,
#                        or src/whisper-package as a CI companion)
# Output: dist/openplayx-asr
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
pkg="${OPX_WHISPER_PACKAGE:-}"
for candidate in "$pkg" "$root/../whisper-package" "$root/../src/whisper-package"; do
  if [ -n "$candidate" ] && [ -f "$candidate/src/opx_asr.h" ]; then pkg="$(cd "$candidate" && pwd)"; break; fi
done
[ -f "$pkg/src/opx_asr.h" ] || { echo "whisper-package not found (set OPX_WHISPER_PACKAGE)" >&2; exit 1; }
"$pkg/tool/build_host_lib.sh" >/dev/null
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) target=macos-arm64 ;;
  Darwin-x86_64) target=macos-x86_64 ;;
  Linux-x86_64) target=linux-x86_64 ;;
  Linux-aarch64) target=linux-arm64 ;;
  *) echo "unsupported host" >&2; exit 1 ;;
esac
version="$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)"
mkdir -p "$root/dist"
cd "$root/src"
CGO_ENABLED=1 \
CGO_CFLAGS="-I$pkg/src" \
CGO_LDFLAGS="-L$pkg/build/host -L$pkg/third_party/whisper/$target/lib" \
  go build -tags asr_native -trimpath -ldflags "-s -w -X main.version=$version" -o "$root/dist/openplayx-asr" .
echo "$root/dist/openplayx-asr"
