#!/bin/bash
# Install or upgrade GoLinx as a systemd service on Linux.
# Safe to run multiple times — detects existing installations, prompts before
# overwriting config, and always updates the binary, service file, and readme.
#
# Usage:  curl -fsSL https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install-service.sh | sudo bash
set -e

REPO="staceyw/GoLinx"
BASE_URL="https://github.com/$REPO/releases/latest/download"
BIN_PATH="/usr/local/bin/golinx"
SERVICE_FILE="/etc/systemd/system/golinx.service"

# --- Must be root ------------------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
  echo "Error: This script must be run as root (use sudo)."
  exit 1
fi

# --- Detect architecture -----------------------------------------------------

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)             echo "Error: Unsupported architecture: $ARCH"; exit 1 ;;
esac

BINARY="golinx-linux-${ARCH}"

# --- Helper for reading input (works with curl | sudo bash) ------------------

prompt() {
  printf "%s" "$1"
  if [ -t 0 ]; then
    read -r REPLY
  else
    read -r REPLY < /dev/tty
  fi
}

# --- Detect existing installation -------------------------------------------

EXISTING_DIR=""
EXISTING_USER=""
IS_UPGRADE=false

if [ -f "$SERVICE_FILE" ]; then
  EXISTING_DIR=$(grep -oP '(?<=WorkingDirectory=).+' "$SERVICE_FILE" 2>/dev/null || true)
  EXISTING_USER=$(grep -oP '(?<=User=).+' "$SERVICE_FILE" 2>/dev/null || true)
  if [ -n "$EXISTING_DIR" ]; then
    IS_UPGRADE=true
  fi
fi

# --- Gather configuration ----------------------------------------------------

echo ""
echo "GoLinx Service Installer"
echo "========================"
echo ""

if $IS_UPGRADE; then
  echo "  Existing installation detected:"
  echo "    Data dir: $EXISTING_DIR"
  echo "    Run as:   $EXISTING_USER"
  echo ""
fi

# Data directory — default to existing installation or /home/<user>/golinx
REAL_USER="${SUDO_USER:-$(logname 2>/dev/null || echo root)}"
if [ -n "$EXISTING_DIR" ]; then
  DEFAULT_DIR="$EXISTING_DIR"
else
  DEFAULT_DIR="/home/${REAL_USER}/golinx"
fi

prompt "Data directory (config + database) [$DEFAULT_DIR]: "
DATA_DIR="${REPLY:-$DEFAULT_DIR}"

if [ -d "$DATA_DIR" ]; then
  echo "  OK: $DATA_DIR exists."
elif [ -e "$DATA_DIR" ]; then
  echo "Error: $DATA_DIR exists but is not a directory."
  exit 1
else
  prompt "  $DATA_DIR does not exist. Create it? [Y/n] "
  case "$REPLY" in
    [nN]*) echo "Aborted."; exit 1 ;;
  esac
  mkdir -p "$DATA_DIR"
  echo "  Created: $DATA_DIR"
fi

# Listener
prompt "Listener URI [http://:80]: "
LISTENER="${REPLY:-http://:80}"
LISTENER2=""

# Tailscale hostname (only if ts+* listener)
TS_HOSTNAME=""
case "$LISTENER" in
  ts+https*)
    prompt "Tailscale hostname [go]: "
    TS_HOSTNAME="${REPLY:-go}"
    echo "  Recommended: add ts+http://:80 so go/link works (bare hostnames need HTTP)."
    prompt "  Add ts+http://:80 listener? [Y/n] "
    case "$REPLY" in
      [nN]*) ;;
      *)     LISTENER2="ts+http://:80" ;;
    esac
    ;;
  ts+http*)
    prompt "Tailscale hostname [go]: "
    TS_HOSTNAME="${REPLY:-go}"
    ;;
  https*)
    echo "  Recommended: add http://:80 so go/link works (bare hostnames need HTTP)."
    prompt "  Add http://:80 listener? [Y/n] "
    case "$REPLY" in
      [nN]*) ;;
      *)     LISTENER2="http://:80" ;;
    esac
    ;;
esac

# Run-as user — default to existing or detect from data dir owner
if [ -n "$EXISTING_USER" ]; then
  DETECTED_USER="$EXISTING_USER"
else
  DETECTED_USER=$(stat -c '%U' "$DATA_DIR" 2>/dev/null || stat -f '%Su' "$DATA_DIR" 2>/dev/null || echo "$REAL_USER")
fi
prompt "Run service as user [$DETECTED_USER]: "
RUN_USER="${REPLY:-$DETECTED_USER}"

if ! id "$RUN_USER" >/dev/null 2>&1; then
  echo "Error: User $RUN_USER does not exist."
  exit 1
fi

# --- Confirm ------------------------------------------------------------------

echo ""
if $IS_UPGRADE; then
  echo "Upgrade plan:"
else
  echo "Configuration:"
fi
echo "  Binary:      $BIN_PATH"
echo "  Data dir:    $DATA_DIR"
echo "  Listener:    $LISTENER"
if [ -n "$LISTENER2" ]; then
  echo "               $LISTENER2"
fi
if [ -n "$TS_HOSTNAME" ]; then
  echo "  TS hostname: $TS_HOSTNAME"
fi
echo "  Run as:      $RUN_USER"
echo ""
if $IS_UPGRADE; then
  prompt "Upgrade and restart service? [Y/n] "
else
  prompt "Install and start service? [Y/n] "
fi
case "$REPLY" in
  [nN]*) echo "Aborted."; exit 0 ;;
esac

# --- Stop existing service before upgrading -----------------------------------

if $IS_UPGRADE; then
  echo ""
  echo "Stopping existing service ..."
  systemctl stop golinx 2>/dev/null || true
fi

# --- Download binary ----------------------------------------------------------

echo ""
echo "Downloading $BINARY ..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL -o "$BIN_PATH" "${BASE_URL}/${BINARY}"
elif command -v wget >/dev/null 2>&1; then
  wget -q -O "$BIN_PATH" "${BASE_URL}/${BINARY}"
else
  echo "Error: Neither curl nor wget found."
  exit 1
fi
chmod +x "$BIN_PATH"
echo "  Installed: $BIN_PATH"

# --- Generate or update config ------------------------------------------------

CONFIG_FILE="${DATA_DIR}/golinx.toml"
WRITE_CONFIG=true

if [ -f "$CONFIG_FILE" ]; then
  prompt "  Config exists: $CONFIG_FILE. Overwrite? [y/N] "
  case "$REPLY" in
    [yY]*) echo "  Overwriting config." ;;
    *)     echo "  Keeping existing config."; WRITE_CONFIG=false ;;
  esac
fi

if $WRITE_CONFIG; then
  echo "  Generating: $CONFIG_FILE"
  if [ -n "$LISTENER2" ]; then
    cat > "$CONFIG_FILE" <<TOML
# GoLinx configuration (generated by install-service.sh)

listen = [
  "$LISTENER",
  "$LISTENER2",
]
TOML
  else
    cat > "$CONFIG_FILE" <<TOML
# GoLinx configuration (generated by install-service.sh)

listen = [
  "$LISTENER",
]
TOML
  fi

  if [ -n "$TS_HOSTNAME" ]; then
    echo "ts-hostname = \"$TS_HOSTNAME\"" >> "$CONFIG_FILE"
  fi

  chown "$RUN_USER" "$CONFIG_FILE"
fi

# Ensure data dir is owned by run user
chown "$RUN_USER" "$DATA_DIR"

# --- Always update readme.txt -------------------------------------------------

cat > "${DATA_DIR}/readme.txt" <<EOF
GoLinx - URL shortener and people directory

Managed as a systemd service.

Service commands:
  sudo systemctl status golinx            # check status
  sudo systemctl start golinx             # start
  sudo systemctl stop golinx              # stop
  sudo systemctl restart golinx           # restart
  sudo systemctl enable golinx            # start on boot
  sudo systemctl disable golinx           # do not start on boot

Logs:
  journalctl -u golinx -f                 # follow live logs
  journalctl -u golinx --since "1 hour ago"

List all services:
  systemctl list-units --type=service     # all running services
  systemctl list-units --type=service --state=running
  systemctl list-unit-files --type=service --state=enabled

Paths:
  Config:          ${CONFIG_FILE}
  Binary:          ${BIN_PATH}
  Service file:    ${SERVICE_FILE}

Upgrade:
  curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install-service.sh | sudo bash

Project page:    https://staceyw.github.io/GoLinx
Documentation:   https://github.com/$REPO#documentation
EOF
chown "$RUN_USER" "${DATA_DIR}/readme.txt"
echo "  Updated: ${DATA_DIR}/readme.txt"

# --- Create systemd service ---------------------------------------------------

echo "Creating systemd service ..."

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=GoLinx - URL Shortener & People Directory
After=network.target

[Service]
Type=simple
User=$RUN_USER
WorkingDirectory=$DATA_DIR
ExecStart=$BIN_PATH
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

echo "  Created: $SERVICE_FILE"

# --- Enable and start ---------------------------------------------------------

systemctl daemon-reload
systemctl enable golinx
systemctl start golinx

echo ""
if $IS_UPGRADE; then
  echo "Done! GoLinx has been upgraded and restarted."
else
  echo "Done! GoLinx is running."
fi
echo ""
echo "  Status:   sudo systemctl status golinx"
echo "  Logs:     journalctl -u golinx -f"
echo "  Stop:     sudo systemctl stop golinx"
echo "  Restart:  sudo systemctl restart golinx"
echo ""
echo "Config: $CONFIG_FILE"
echo "To change settings, edit the config file then:"
echo "  sudo systemctl restart golinx"
echo ""
