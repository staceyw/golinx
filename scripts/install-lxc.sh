#!/bin/bash
# Create a Proxmox LXC container with GoLinx installed as a systemd service.
# Usage:  curl -fsSL https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install-lxc.sh | bash
#
# Run this on the Proxmox host (not inside a container). The script will:
#   1. Prompt for container settings (ID, hostname, resources, network, listener)
#   2. Download the Debian 12 template if not already cached
#   3. Create and start an unprivileged LXC container
#   4. Install GoLinx inside the container and start it as a systemd service
set -e

REPO="staceyw/GoLinx"
BASE_URL="https://github.com/$REPO/releases/latest/download"
BIN_PATH="/usr/local/bin/golinx"
DATA_DIR="/root/golinx"
SERVICE_FILE="/etc/systemd/system/golinx.service"

# --- Pre-flight checks -------------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
  echo "Error: This script must be run as root."
  echo "  Usage: curl -fsSL <url> | sudo bash"
  exit 1
fi

if ! command -v pveversion >/dev/null 2>&1; then
  echo "Error: pveversion not found. This script must be run on a Proxmox VE host."
  exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)             echo "Error: Unsupported architecture: $ARCH"; exit 1 ;;
esac

BINARY="golinx-linux-${ARCH}"

# --- Helper for reading input (works with curl | bash) -----------------------

prompt() {
  printf "%s" "$1"
  if [ -t 0 ]; then
    read -r REPLY
  else
    read -r REPLY < /dev/tty
  fi
}

# --- Gather configuration ----------------------------------------------------

echo ""
echo "GoLinx Proxmox LXC Installer"
echo "============================"
echo ""

# Container ID
DEFAULT_CTID=$(pvesh get /cluster/nextid 2>/dev/null || echo "100")
prompt "Container ID [$DEFAULT_CTID]: "
CTID="${REPLY:-$DEFAULT_CTID}"

# Validate CTID is a number
case "$CTID" in
  ''|*[!0-9]*) echo "Error: Container ID must be a number."; exit 1 ;;
esac

# Check if CTID already exists
if pct status "$CTID" >/dev/null 2>&1; then
  echo "Error: Container $CTID already exists."
  exit 1
fi

# Hostname
prompt "Hostname [golinx]: "
CT_HOSTNAME="${REPLY:-golinx}"

# Root password
echo "Set a root password for console/SSH access (leave blank to skip)."
printf "Root password: "
if [ -t 0 ]; then
  stty -echo 2>/dev/null
  read -r ROOT_PASS
  stty echo 2>/dev/null
else
  stty -echo 2>/dev/null </dev/tty
  read -r ROOT_PASS </dev/tty
  stty echo 2>/dev/null </dev/tty
fi
echo ""

# Resources
prompt "Memory in MB [256]: "
MEMORY="${REPLY:-256}"

prompt "Disk size in GB [2]: "
DISK="${REPLY:-2}"

# Storage
prompt "Storage target [local-lvm]: "
STORAGE="${REPLY:-local-lvm}"

# Network
prompt "Network bridge [vmbr0]: "
BRIDGE="${REPLY:-vmbr0}"

prompt "IP config - (d)hcp or (s)tatic [dhcp]: "
IP_MODE="${REPLY:-dhcp}"

if [ "$IP_MODE" = "s" ] || [ "$IP_MODE" = "static" ]; then
  prompt "IP address (CIDR, e.g. 192.168.1.50/24): "
  STATIC_IP="$REPLY"
  if [ -z "$STATIC_IP" ]; then
    echo "Error: Static IP is required."
    exit 1
  fi
  prompt "Gateway (e.g. 192.168.1.1): "
  GATEWAY="$REPLY"
  if [ -z "$GATEWAY" ]; then
    echo "Error: Gateway is required for static IP."
    exit 1
  fi
  NET_CONFIG="name=eth0,bridge=${BRIDGE},ip=${STATIC_IP},gw=${GATEWAY}"
else
  NET_CONFIG="name=eth0,bridge=${BRIDGE},ip=dhcp"
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

# --- Confirm ------------------------------------------------------------------

echo ""
echo "Configuration:"
echo "  Container ID: $CTID"
echo "  Hostname:     $CT_HOSTNAME"
echo "  Root password: $([ -n "$ROOT_PASS" ] && echo '(set)' || echo '(none)')"
echo "  Memory:       ${MEMORY} MB"
echo "  Disk:         ${DISK} GB"
echo "  Storage:      $STORAGE"
echo "  Network:      $NET_CONFIG"
echo "  Listener:     $LISTENER"
if [ -n "$LISTENER2" ]; then
  echo "                $LISTENER2"
fi
if [ -n "$TS_HOSTNAME" ]; then
  echo "  TS hostname:  $TS_HOSTNAME"
fi
echo "  Architecture: $ARCH"
echo ""
prompt "Create container and install GoLinx? [Y/n] "
case "$REPLY" in
  [nN]*) echo "Aborted."; exit 0 ;;
esac

# --- Download Debian template if needed ---------------------------------------

echo ""
echo "Checking for Debian 12 template ..."
pveam update >/dev/null 2>&1 || true

TEMPLATE=$(pveam available --section system 2>/dev/null | awk '/debian-12-standard/ {print $2}' | sort -V | tail -n1)
if [ -z "$TEMPLATE" ]; then
  echo "Error: Could not find a Debian 12 template. Run 'pveam update' and retry."
  exit 1
fi

TEMPLATE_PATH="/var/lib/vz/template/cache/${TEMPLATE}"
if [ -f "$TEMPLATE_PATH" ]; then
  echo "  Template cached: $TEMPLATE"
else
  echo "  Downloading: $TEMPLATE ..."
  pveam download local "$TEMPLATE"
fi

# Use the local storage path for template reference
TEMPLATE_REF="local:vztmpl/${TEMPLATE}"

# --- Create LXC container ----------------------------------------------------

echo ""
echo "Creating container $CTID ..."
pct create "$CTID" "$TEMPLATE_REF" \
  --hostname "$CT_HOSTNAME" \
  --memory "$MEMORY" \
  --swap 0 \
  --cores 1 \
  --rootfs "${STORAGE}:${DISK}" \
  --net0 "$NET_CONFIG" \
  --unprivileged 1 \
  --features nesting=1 \
  --start 0 \
  --ostype debian

echo "  Created: CT $CTID ($CT_HOSTNAME)"

# --- Start container ----------------------------------------------------------

echo "Starting container $CTID ..."
pct start "$CTID"

# Wait for network connectivity
echo "  Waiting for network ..."
for i in $(seq 1 15); do
  if pct exec "$CTID" -- bash -c "ping -c1 -W1 github.com >/dev/null 2>&1"; then
    break
  fi
  sleep 1
done

# Set root password if provided
if [ -n "$ROOT_PASS" ]; then
  pct exec "$CTID" -- bash -c "echo 'root:$ROOT_PASS' | chpasswd"
  echo "  Root password set"
fi

# --- Install GoLinx inside container -----------------------------------------

echo ""
echo "Installing GoLinx inside container ..."

# Install curl
pct exec "$CTID" -- bash -c "apt-get update -qq && apt-get install -y -qq curl >/dev/null 2>&1"
echo "  Installed curl"

# Download binary
echo "  Downloading $BINARY ..."
pct exec "$CTID" -- bash -c "curl -fsSL -o $BIN_PATH '${BASE_URL}/${BINARY}' && chmod +x $BIN_PATH"
echo "  Installed: $BIN_PATH"

# Create data directory
pct exec "$CTID" -- mkdir -p "$DATA_DIR"

# Generate config
echo "  Generating config ..."
LISTEN_LINES="  \"${LISTENER}\","
if [ -n "$LISTENER2" ]; then
  LISTEN_LINES="${LISTEN_LINES}
  \"${LISTENER2}\","
fi

CONFIG_CONTENT="# GoLinx configuration (generated by install-lxc.sh)

listen = [
${LISTEN_LINES}
]"

if [ -n "$TS_HOSTNAME" ]; then
  CONFIG_CONTENT="${CONFIG_CONTENT}
ts-hostname = \"${TS_HOSTNAME}\""
fi

pct exec "$CTID" -- bash -c "cat > ${DATA_DIR}/golinx.toml << 'INNEREOF'
${CONFIG_CONTENT}
INNEREOF"

# Create systemd service
echo "  Creating systemd service ..."
pct exec "$CTID" -- bash -c "cat > $SERVICE_FILE << 'INNEREOF'
[Unit]
Description=GoLinx - URL Shortener & People Directory
After=network.target

[Service]
Type=simple
WorkingDirectory=${DATA_DIR}
ExecStart=${BIN_PATH}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
INNEREOF"

# Generate local readme
pct exec "$CTID" -- bash -c "cat > ${DATA_DIR}/readme.txt << 'INNEREOF'
GoLinx - URL shortener and people directory

Managed as a systemd service. Common commands (run from Proxmox host):

  pct exec $CTID -- systemctl status golinx       # check status
  pct exec $CTID -- systemctl restart golinx      # restart
  pct exec $CTID -- journalctl -u golinx -f       # view logs

Config:          ${DATA_DIR}/golinx.toml
Binary:          ${BIN_PATH}

Project page:    https://staceyw.github.io/GoLinx
Documentation:   https://github.com/staceyw/GoLinx#documentation
INNEREOF"

# Enable and start
pct exec "$CTID" -- systemctl daemon-reload
pct exec "$CTID" -- systemctl enable --now golinx

# --- Done ---------------------------------------------------------------------

echo ""
CT_IP=$(pct exec "$CTID" -- hostname -I 2>/dev/null | awk '{print $1}')

echo "Done! GoLinx is running in container $CTID."
echo ""
if [ -n "$CT_IP" ]; then
  echo "  Open: http://${CT_IP}"
fi
echo ""
echo "  Container:"
echo "    pct enter $CTID                                      # shell into container"
echo "    pct stop $CTID                                       # stop container"
echo "    pct start $CTID                                      # start container"
echo ""
echo "  Service (from host):"
echo "    pct exec $CTID -- systemctl status golinx            # check status"
echo "    pct exec $CTID -- journalctl -u golinx -f            # view logs"
echo "    pct exec $CTID -- systemctl restart golinx           # restart"
echo ""
echo "  Config: ${DATA_DIR}/golinx.toml (inside container)"
echo "  To change settings, edit the config then restart the service."
echo ""

if [ -n "$TS_HOSTNAME" ]; then
  echo "  NOTE: Tailscale listener selected. You need to install Tailscale inside"
  echo "  the container separately:"
  echo "    pct enter $CTID"
  echo "    curl -fsSL https://tailscale.com/install.sh | sh"
  echo "    tailscale up"
  echo ""
fi
