#!/usr/bin/env bash
# Install and patch a kimi-code npm release so it honours KIMI_SHELL_PATH.
#
# Stock kimi-code resolves its POSIX shell from a hardcoded candidate list
# (/bin/bash, /usr/bin/bash, ...) and spawns it by absolute path, which
# bypasses reach's PATH shim entirely.  contrib/kimi-shell-path-patch.mjs
# prepends process.env.KIMI_SHELL_PATH to that candidate list, giving reach
# its intercept point.
#
# Usage:
#   contrib/kimi-install-and-patch.sh [VERSION]
#
# VERSION defaults to the latest published @moonshot-ai/kimi-code.
#
# Installation target:
#   ~/.reach/kimi-<VERSION>/node_modules/.bin/kimi
#
# This is the path reach's resolveKimiBinary() searches when deciding which
# kimi binary to launch.  After this script completes, `reach kimi` will find
# and use the patched binary automatically.
#
# Post-install check:
#   If `reach` is in PATH and REACH_SESSION is set (or a session name is given
#   as the second argument), the script runs `reach harness verify kimi` to
#   confirm the seam is working end-to-end.  Pass --no-verify to skip this.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PATCH_SCRIPT="$SCRIPT_DIR/kimi-shell-path-patch.mjs"

die()  { printf 'error: %s\n' "$1" >&2; exit 1; }
info() { printf '\033[1m%s\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
warn() { printf '  \033[33m⚠\033[0m %s\n' "$1"; }

# ── Argument parsing ──────────────────────────────────────────────────────────

VERSION=""
SESSION=""
NO_VERIFY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-verify)   NO_VERIFY=1; shift ;;
    --session)     SESSION="$2"; shift 2 ;;
    --session=*)   SESSION="${1#--session=}"; shift ;;
    -*)            die "unknown flag $1" ;;
    *)
      if [[ -z "$VERSION" ]]; then VERSION="$1"; shift
      elif [[ -z "$SESSION" ]]; then SESSION="$1"; shift
      else die "unexpected argument $1"
      fi ;;
  esac
done

# ── Prerequisites ─────────────────────────────────────────────────────────────

command -v node >/dev/null 2>&1 || die "node is not in PATH (install Node.js 18+)"
command -v npm  >/dev/null 2>&1 || die "npm is not in PATH"
[[ -f "$PATCH_SCRIPT" ]] || die "patch script not found at $PATCH_SCRIPT"

# ── Resolve version ───────────────────────────────────────────────────────────

if [[ -z "$VERSION" ]]; then
  info "Resolving latest @moonshot-ai/kimi-code version..."
  VERSION="$(npm view @moonshot-ai/kimi-code version 2>/dev/null)" \
    || die "could not query npm for @moonshot-ai/kimi-code — check your network or npm registry"
  info "Latest: $VERSION"
fi

# ── Install ───────────────────────────────────────────────────────────────────

REACH_HOME="${REACH_HOME:-$HOME/.reach}"
INSTALL_DIR="$REACH_HOME/kimi-$VERSION"

info "Installing @moonshot-ai/kimi-code@$VERSION into $INSTALL_DIR ..."

if [[ -d "$INSTALL_DIR/node_modules/.bin" ]] && \
   [[ -f "$INSTALL_DIR/node_modules/@moonshot-ai/kimi-code/dist/main.mjs" ]]; then
  warn "Already installed at $INSTALL_DIR — re-running patch only."
  warn "To force a fresh install, remove $INSTALL_DIR first."
else
  mkdir -p "$INSTALL_DIR"
  npm install \
    --prefix "$INSTALL_DIR" \
    --no-save \
    --loglevel warn \
    "@moonshot-ai/kimi-code@$VERSION"
  ok "npm install complete"
fi

# ── Patch ─────────────────────────────────────────────────────────────────────

BUNDLE="$INSTALL_DIR/node_modules/@moonshot-ai/kimi-code/dist/main.mjs"
[[ -f "$BUNDLE" ]] || die "bundle not found at $BUNDLE — is this the right version?"

info "Applying KIMI_SHELL_PATH patch to $BUNDLE ..."
node "$PATCH_SCRIPT" "$BUNDLE"
ok "Patch applied"

# ── Verify binary is reachable ────────────────────────────────────────────────

KIMI_BIN="$INSTALL_DIR/node_modules/.bin/kimi"
[[ -x "$KIMI_BIN" ]] || die "kimi binary not found at $KIMI_BIN after install"

KIMI_VERSION="$("$KIMI_BIN" --version 2>/dev/null | head -1 || true)"
ok "kimi binary: $KIMI_BIN${KIMI_VERSION:+ ($KIMI_VERSION)}"

printf '\n'
info "Installation complete."
printf '  Patched binary: %s\n' "$KIMI_BIN"
printf '  reach will use this binary automatically (resolveKimiBinary prefers\n'
printf '  the newest reach-managed install over the PATH-resolved one).\n'
printf '\n'

# ── Post-install seam verification ───────────────────────────────────────────

if [[ $NO_VERIFY -eq 1 ]]; then
  warn "Skipping seam verification (--no-verify)."
  exit 0
fi

REACH_BIN="$(command -v reach 2>/dev/null || true)"
if [[ -z "$REACH_BIN" ]]; then
  warn "reach not in PATH — skipping seam verification."
  warn "Run 'reach harness verify kimi' after adding reach to PATH."
  exit 0
fi

SESS_ARG=""
SESS="${SESSION:-${REACH_SESSION:-}}"
if [[ -n "$SESS" ]]; then
  SESS_ARG="--session $SESS"
elif ! "$REACH_BIN" exec --session "" -- true >/dev/null 2>&1; then
  # No active session; skip the probe rather than failing confusingly.
  warn "No active reach session — skipping seam verification."
  warn "Start a session with 'reach up <target>' and then run:"
  warn "  reach harness verify kimi"
  exit 0
fi

info "Verifying kimi seam end-to-end against the session target ..."
# shellcheck disable=SC2086
"$REACH_BIN" harness verify kimi \
    --binary "$KIMI_BIN" \
    $SESS_ARG \
    && ok "Seam verdict: ok — shell commands route to the target." \
    || { warn "Seam probe failed or verdict BYPASSED."; exit 1; }
