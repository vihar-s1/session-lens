#!/usr/bin/env bash
# autodeploy.sh — rebuilds session-lens binaries from source and restarts the
# server LaunchAgent. Intended to be run unattended by a LaunchAgent every 48h
# (StartInterval=172800). Writes a single timestamped line per run to the log.
#
# Pre-requisites (one-time):
#   1. Binaries installed at ~/.local/bin/sessionlens-{server,hook,scan}
#   2. /usr/local/bin/sessionlens-{server,hook,scan} are symlinks → ~/.local/bin/...
#      (or settings.json and the server plist point directly at ~/.local/bin/)
#   3. com.viharshah.sessionlens LaunchAgent is loaded.
#
# Failure modes are logged but never fatal — the LaunchAgent keeps retrying
# every 48h regardless. A bad build leaves the running binary untouched.

set -u

REPO="/Users/viharshah/Desktop/claude-playground/session-lens"
BIN_DIR="/Users/viharshah/.local/bin"
LOG="/Users/viharshah/Library/Logs/sessionlens-autodeploy.log"
PATH="/usr/local/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

ts() { date "+%Y-%m-%d %H:%M:%S"; }
log() { echo "[$(ts)] $*" >> "$LOG"; }

log "autodeploy run start"

cd "$REPO" || { log "FAIL: cd $REPO"; exit 0; }

# Build into a staging dir; only swap on success so a broken build never
# clobbers a working binary.
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

if ! go build -o "$STAGE/sessionlens-server" ./cmd/sessionlens-server 2>>"$LOG"; then
  log "FAIL: build server"
  exit 0
fi
if ! go build -o "$STAGE/sessionlens-hook" ./cmd/sessionlens-hook 2>>"$LOG"; then
  log "FAIL: build hook"
  exit 0
fi
if ! go build -o "$STAGE/sessionlens-scan" ./cmd/sessionlens-scan 2>>"$LOG"; then
  log "FAIL: build scan"
  exit 0
fi

# Atomic-ish replace: mv is atomic on the same filesystem.
mkdir -p "$BIN_DIR"
mv -f "$STAGE/sessionlens-server" "$BIN_DIR/sessionlens-server" || { log "FAIL: install server"; exit 0; }
mv -f "$STAGE/sessionlens-hook"   "$BIN_DIR/sessionlens-hook"   || { log "FAIL: install hook";   exit 0; }
mv -f "$STAGE/sessionlens-scan"   "$BIN_DIR/sessionlens-scan"   || { log "FAIL: install scan";   exit 0; }

# Restart the server LaunchAgent so the new binary is loaded. The hook is
# spawned fresh per Stop event, so no restart needed.
UID_NUM="$(id -u)"
if ! launchctl kickstart -k "gui/$UID_NUM/com.viharshah.sessionlens" 2>>"$LOG"; then
  log "FAIL: launchctl kickstart"
  exit 0
fi

# NOTE: no self-sync of this script. macOS TCC blocks cp from ~/Desktop/ when
# invoked from a launchd-spawned process (go build is fine — different sandbox
# class). After editing this file in the repo, manually propagate with:
#   cp scripts/autodeploy.sh ~/.local/bin/sessionlens-autodeploy.sh
# from a regular terminal session.

log "autodeploy run ok (server + hook + scan rebuilt + restarted)"
