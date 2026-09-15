#!/usr/bin/env bash
# refresh-bundle — rebuild ONLY the Go server binary, install it into BOTH the
# menubar app bundle and ~/.local/bin, then restart the running server via the
# canonical `claude-wall restart`. Use after changing server code / static
# assets (m.html, pip.html, …) when you don't need to recompile the Swift app.
#
# Restart is delegated to `claude-wall restart` on purpose: the server writes a
# pidfile on bind (so restart finds it whether the menubar app or the CLI
# spawned it) and refuses to start when the port is already held — so even if
# the app's terminationHandler respawns its own copy at the same time, at most
# one wins and no invisible second server attaches to tmux.
#
# Usage: macapp/refresh-bundle.sh [port]   (default 7685)
set -euo pipefail
cd "$(dirname "$0")"

PORT="${1:-7685}"
BUNDLE_BIN="Claude Wall.app/Contents/Resources/claude-wall"
LOCAL_BIN="$HOME/.local/bin/claude-wall"
BUILD="$(mktemp -t claude-wall.XXXXXX)"

echo "building server binary…"
( cd .. && go build -o "$BUILD" . )
codesign --force -s - "$BUILD" 2>/dev/null || true

# Atomic replace (rename) so a running process keeps its old inode — no "text
# file busy". cp to a sibling .new first (cp into a busy file would fail), then
# mv over.
replace() {
  local target="$1" dir
  dir="$(dirname "$target")"
  [ -d "$dir" ] || { echo "  skip $target (dir missing)"; return; }
  cp "$BUILD" "$target.new"
  codesign --force -s - "$target.new" 2>/dev/null || true
  mv "$target.new" "$target"
  echo "  updated $target"
}
echo "installing fresh binary…"
replace "$LOCAL_BIN"
replace "$BUNDLE_BIN"
rm -f "$BUILD"

# Preserve the current bind mode: --public binds 0.0.0.0 for Tailscale/remote.
PUB=""
PID="$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
if [ -n "$PID" ] && ps -o command= -p "$PID" 2>/dev/null | grep -q -- '--public'; then
  PUB="--public"
fi

echo "restarting server${PUB:+ (public)} via claude-wall restart…"
"$LOCAL_BIN" restart ${PUB} --port "$PORT" || true

# Give whichever server won the bind a moment to come up.
for i in $(seq 1 8); do
  sleep 1
  [ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://localhost:$PORT/api/health" 2>/dev/null || true)" = "200" ] && break
done

MCODE="$(curl -s -m 4 -o /dev/null -w '%{http_code}' "http://localhost:$PORT/m.html" 2>/dev/null || true)"
if [ "$MCODE" = "200" ]; then
  echo "✓ live on :$PORT  (m.html $MCODE) — reload the PWA on your phone"
else
  echo "⚠ m.html returned $MCODE on :$PORT — open Claude Wall.app or run: $LOCAL_BIN restart${PUB:+ --public}"
fi
