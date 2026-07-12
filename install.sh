#!/usr/bin/env bash
# Install usher from a GitHub release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/nexustar/usher/main/install.sh | bash
#
# What it does:
#   1. Downloads the latest release binary to ~/.local/bin/usher (override with USHER_INSTALL_DIR)
#   2. Installs a user-level service (launchd on macOS, systemd on Linux)
#
# To uninstall:
#   usher setup --remove
#   # macOS:  launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/io.github.nexustar.usher.plist
#   # Linux:  systemctl --user disable --now usher
#   rm ~/.local/bin/usher

set -euo pipefail

REPO="nexustar/usher"
INSTALL_DIR="${USHER_INSTALL_DIR:-$HOME/.local/bin}"

# --- helpers ----------------------------------------------------------------

die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
info() { printf '\033[34m==>\033[0m %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found"
}

# --- detect platform --------------------------------------------------------

detect_platform() {
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"

  case "$os" in
    darwin) ;;
    linux)  ;;
    *)      die "unsupported OS: $os" ;;
  esac

  case "$arch" in
    x86_64)       arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *)            die "unsupported architecture: $arch" ;;
  esac

  PLATFORM="${os}-${arch}"
}

# --- download ---------------------------------------------------------------

download_binary() {
  need curl

  local url="https://github.com/${REPO}/releases/latest/download/usher-${PLATFORM}"
  mkdir -p "$INSTALL_DIR"

  info "Downloading usher-${PLATFORM} → ${INSTALL_DIR}/usher"
  curl -fSL --progress-bar "$url" -o "${INSTALL_DIR}/usher"
  chmod +x "${INSTALL_DIR}/usher"
}

# --- ensure PATH ------------------------------------------------------------

ensure_path() {
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) return ;;
  esac
  export PATH="${INSTALL_DIR}:${PATH}"

  local line="export PATH=\"${INSTALL_DIR}:\$PATH\""
  local rc=""
  case "${SHELL:-}" in
    */zsh)  rc="$HOME/.zshrc"   ;;
    */bash) rc="$HOME/.bashrc"  ;;
  esac
  if [ -z "$rc" ] || [ ! -f "$rc" ]; then
    for f in "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.zshrc" "$HOME/.zprofile" "$HOME/.profile"; do
      [ -f "$f" ] && rc="$f" && break
    done
  fi
  if [ -n "$rc" ]; then
    grep -qF "$INSTALL_DIR" "$rc" 2>/dev/null || printf '\n%s\n' "$line" >> "$rc"
    info "Added ${INSTALL_DIR} to PATH in ${rc}"
  else
    info "Add to your shell profile: ${line}"
  fi
}

# --- service install --------------------------------------------------------

install_service_darwin() {
  local plist_dir="$HOME/Library/LaunchAgents"
  local plist="${plist_dir}/io.github.nexustar.usher.plist"
  local bin="${INSTALL_DIR}/usher"

  mkdir -p "$plist_dir"

  # Collect PATH dirs that contain claude / codex / usher.
  local svc_path=""
  for cmd in claude codex; do
    local p
    p="$(command -v "$cmd" 2>/dev/null || true)"
    [ -n "$p" ] && svc_path="${svc_path:+${svc_path}:}$(dirname "$p")"
  done
  svc_path="${svc_path:+${svc_path}:}${INSTALL_DIR}:/usr/bin:/bin"

  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>io.github.nexustar.usher</string>
  <key>ProgramArguments</key>
  <array>
    <string>${bin}</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/usher.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/usher.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${svc_path}</string>
  </dict>
</dict>
</plist>
EOF

  launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  info "LaunchAgent installed and started"
  info "  Logs: /tmp/usher.log"
  info "  Stop: launchctl bootout gui/$(id -u) ${plist}"
}

install_service_linux() {
  local unit_dir="$HOME/.config/systemd/user"
  local unit="${unit_dir}/usher.service"
  local bin="${INSTALL_DIR}/usher"

  need systemctl

  # Collect PATH dirs that contain claude / codex / usher.
  local svc_path=""
  for cmd in claude codex; do
    local p
    p="$(command -v "$cmd" 2>/dev/null || true)"
    [ -n "$p" ] && svc_path="${svc_path:+${svc_path}:}$(dirname "$p")"
  done
  svc_path="${svc_path:+${svc_path}:}${INSTALL_DIR}:/usr/bin:/bin"

  mkdir -p "$unit_dir"

  cat > "$unit" <<EOF
[Unit]
Description=usher — Claude/Codex session router
After=network.target

[Service]
Type=simple
ExecStart=${bin} serve
Restart=on-failure
RestartSec=3
Environment=PATH=${svc_path}

[Install]
WantedBy=default.target
EOF

  systemctl --user daemon-reload
  systemctl --user enable --now usher
  if [ "$(loginctl show-user "$USER" --property=Linger 2>/dev/null)" != "Linger=yes" ]; then
    info "To keep usher running after logout: sudo loginctl enable-linger $USER"
  fi
  info "systemd user service installed and started"
  info "  Logs: journalctl --user -u usher -f"
  info "  Stop: systemctl --user disable --now usher"
}

install_service() {
  case "$(uname -s)" in
    Darwin) install_service_darwin ;;
    Linux)  install_service_linux  ;;
  esac
}

# --- main -------------------------------------------------------------------

main() {
  detect_platform
  download_binary
  ensure_path
  install_service

  echo
  info "Done! usher is running on http://127.0.0.1:7777"
  info "Set a password for remote access: usher set-password"
}

main "$@"
