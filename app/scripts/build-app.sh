#!/usr/bin/env bash
# Builds, signs and verifies Artefact.app, which opens artefact:// links.
#   scripts/build-app.sh [--install]
# --install   replaces /Applications/Artefact.app and registers the artefact: scheme
set -euo pipefail

APP_DIR="$(cd "$(dirname "$0")/.." && pwd)"
IDENTITY="${ARTEFACT_SIGN_IDENTITY:?set ARTEFACT_SIGN_IDENTITY to your signing identity, e.g. Developer ID Application: Jane Doe (TEAMID)}"
INSTALL=0
for arg in "$@"; do
  case "$arg" in
    --install) INSTALL=1 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

cd "$APP_DIR"
swift build -c release
BIN="$(swift build -c release --show-bin-path)/Artefact"

OUT="$APP_DIR/.build/Artefact.app"
rm -rf "$OUT"
mkdir -p "$OUT/Contents/MacOS" "$OUT/Contents/Resources"
cp "$BIN" "$OUT/Contents/MacOS/Artefact"
cp Resources/Info.plist "$OUT/Contents/Info.plist"

codesign --sign "$IDENTITY" --options runtime --timestamp --force "$OUT"
codesign --verify --deep --strict --verbose=2 "$OUT"

if [ "$INSTALL" = 1 ]; then
  rm -rf /Applications/Artefact.app
  ditto "$OUT" /Applications/Artefact.app
  /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f /Applications/Artefact.app
  echo "installed /Applications/Artefact.app"
else
  echo "built $OUT"
fi
