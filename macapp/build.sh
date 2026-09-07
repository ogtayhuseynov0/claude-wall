#!/usr/bin/env bash
# Build "Claude Wall.app" — a single native menubar app that OWNS the claude-wall
# server (bundled binary, spawned on launch) and adds PiP windows, a floating
# button, and a stats popover. Run from claude-wall/macapp.
set -euo pipefail
cd "$(dirname "$0")"

APP="Claude Wall.app"
EXE="ClaudeWall"

echo "building claude-wall server binary..."
( cd .. && go build -o macapp/claude-wall . )

echo "generating icon..."
swiftc -O makeicon.swift -o makeicon -framework Cocoa
./makeicon
iconutil -c icns AppIcon.iconset -o AppIcon.icns
rm -rf AppIcon.iconset makeicon

echo "compiling app..."
swiftc -O -swift-version 5 main.swift -o "${EXE}" -framework Cocoa -framework WebKit -framework Carbon

echo "bundling ${APP}..."
rm -rf "${APP}"
mkdir -p "${APP}/Contents/MacOS" "${APP}/Contents/Resources"
mv "${EXE}" "${APP}/Contents/MacOS/${EXE}"
mv claude-wall "${APP}/Contents/Resources/claude-wall"   # bundled server
cp Info.plist "${APP}/Contents/Info.plist"
cp AppIcon.icns "${APP}/Contents/Resources/AppIcon.icns"

# ad-hoc sign so macOS runs it locally without Gatekeeper fuss
codesign --force --deep --sign - "${APP}" >/dev/null 2>&1 || true

echo "built $(pwd)/${APP}"
