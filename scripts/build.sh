#!/bin/sh
# Builds every release artifact into dist/.
#   scripts/build.sh            version from `git describe`, or "dev"
#   VERSION=1.2.3 scripts/build.sh
# The macOS .app needs lipo/codesign (run on macOS); everything else
# cross-compiles from any OS with Go installed.
set -eu
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null | sed 's/^v//' || echo dev)}
LDFLAGS="-s -w -X main.version=$VERSION"
PKG=./cmd/disk-recovery-lab
rm -rf dist && mkdir -p dist
export CGO_ENABLED=0

for arch in amd64 arm64; do
  name=$arch; [ $arch = amd64 ] && name=x64
  echo "linux/$arch"
  GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "dist/disk-recovery-lab-linux-$name" $PKG
  echo "windows/$arch"
  GOOS=windows GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS -H windowsgui" -o "dist/DiskRecoveryLab-windows-$name.exe" $PKG
done

if command -v lipo >/dev/null 2>&1; then
  echo "macOS universal"
  T=$(mktemp -d)
  for arch in amd64 arm64; do
    GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$T/drl-$arch" $PKG
  done
  APP="$T/Disk Recovery Lab.app"
  mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
  lipo -create -output "$APP/Contents/MacOS/disk-recovery-lab" "$T/drl-amd64" "$T/drl-arm64"
  sed "s/__VERSION__/$VERSION/g" packaging/macos/Info.plist > "$APP/Contents/Info.plist"
  cp packaging/macos/AppIcon.icns "$APP/Contents/Resources/"
  codesign --force --deep --sign - "$APP"   # ad-hoc signature (required on Apple Silicon)
  (cd "$T" && ditto -c -k --keepParent "Disk Recovery Lab.app" app.zip)
  mv "$T/app.zip" "dist/DiskRecoveryLab-macos-universal.zip"
  rm -rf "$T"
else
  echo "skipping macOS app (needs lipo and codesign, i.e. macOS)"
fi

(cd dist && shasum -a 256 * > SHA256SUMS 2>/dev/null || sha256sum * > SHA256SUMS)
ls -l dist
