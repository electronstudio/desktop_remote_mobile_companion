#!/bin/sh
# Build inara-x86_64-<version>.AppImage from an already-built inara_gui
# binary (locally: `make inara_gui`; CI: the artifact of the build-linux
# job). The AppDir layout:
#
#   AppRun                                   entry point (cap bootstrap, see file)
#   inara.desktop                            desktop file
#   inara.png                                icon
#   usr/bin/inara_gui                        the binary (static FFmpeg)
#   usr/share/metainfo/...metainfo.xml       AppStream metadata
#
# Usage: make-appimage.sh <path-to-inara_gui> <version>

set -eu

usage() {
    echo "usage: $0 <path-to-inara_gui> <version>" >&2
    exit 2
}
[ $# -eq 2 ] || usage
BIN=$1
VERSION=$2
ARCH=x86_64

# Pinned appimagetool release. Downloaded once and cached under build/.
APPIMAGETOOL_VERSION=1.9.0

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
SRC=$ROOT/packaging/appimage
TOOL=$ROOT/build/appimagetool-$APPIMAGETOOL_VERSION
DIST=$ROOT/dist
OUT=$DIST/inara-$ARCH-$VERSION.AppImage

[ -x "$BIN" ] || {
    echo "$0: $BIN not found or not executable; run 'make inara_gui' first" >&2
    exit 1
}

if [ ! -x "$TOOL" ]; then
    echo "Downloading appimagetool $APPIMAGETOOL_VERSION"
    mkdir -p "$(dirname "$TOOL")"
    curl -fL --retry 3 -o "$TOOL" \
        "https://github.com/AppImage/appimagetool/releases/download/$APPIMAGETOOL_VERSION/appimagetool-$ARCH.AppImage"
    chmod +x "$TOOL"
fi

APPDIR_PARENT=$(mktemp -d)
APPDIR=$APPDIR_PARENT/inara.AppDir
# shellcheck disable=SC2064
trap "rm -rf '$APPDIR_PARENT'" EXIT

install -Dm755 "$SRC/AppRun" "$APPDIR/AppRun"
install -Dm644 "$SRC/inara.desktop" "$APPDIR/inara.desktop"
install -Dm644 "$ROOT/server/static/icon-512.png" "$APPDIR/inara.png"
install -Dm755 "$BIN" "$APPDIR/usr/bin/inara_gui"
mkdir -p "$APPDIR/usr/share/metainfo"
sed -e "s/@VERSION@/$VERSION/" -e "s/@DATE@/$(date +%F)/" \
    "$SRC/uk.co.electronstudio.inara.metainfo.xml" \
    >"$APPDIR/usr/share/metainfo/uk.co.electronstudio.inara.metainfo.xml"

# Validate what we can with whatever is installed. The desktop file is
# cheap to check and a broken one breaks appimagetool, so that one is
# fatal; AppStream validation (if available) is reported but non-fatal.
if command -v desktop-file-validate >/dev/null 2>&1; then
    desktop-file-validate "$APPDIR/inara.desktop"
fi
if command -v appstreamcli >/dev/null 2>&1; then
    appstreamcli validate --no-net \
        "$APPDIR/usr/share/metainfo/uk.co.electronstudio.inara.metainfo.xml" || true
fi

mkdir -p "$DIST"
# appimagetool is itself an AppImage; CI containers and some systems lack
# FUSE, so run its embedded payload directly instead of mounting.
export ARCH
export APPIMAGE_EXTRACT_AND_RUN=1
"$TOOL" "$APPDIR" "$OUT"

echo "Built $OUT"
