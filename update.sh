#!/bin/bash
set -e

echo "Updating System Metrics Exporter..."

# Check if running as root
if [ "$EUID" -ne 0 ]; then
  echo "Please run this script as root (e.g., sudo ./update.sh)"
  exit 1
fi

# Determine system architecture
ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    BIN_NAME="system_exporter_linux_amd64"
elif [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    BIN_NAME="system_exporter_linux_arm64"
else
    echo "Unsupported architecture: $ARCH"
    exit 1
fi

DOWNLOAD_URL="https://raw.githubusercontent.com/HarpalPannu/go-system-exporter/main/bin/$BIN_NAME"
INSTALL_DIR="/opt/system_exporter"
BIN_PATH="$INSTALL_DIR/system_exporter"

if [ ! -d "$INSTALL_DIR" ]; then
  echo "Error: Installation directory $INSTALL_DIR not found."
  echo "Is the exporter installed as a systemd service?"
  exit 1
fi

echo "Downloading latest binary for $ARCH..."
wget -qO /tmp/system_exporter_update "$DOWNLOAD_URL"
chmod +x /tmp/system_exporter_update

echo "Stopping system-exporter service..."
systemctl stop system-exporter.service || echo "Warning: Service was not running."

echo "Installing new binary..."
mv /tmp/system_exporter_update "$BIN_PATH"

echo "Starting system-exporter service..."
systemctl start system-exporter.service

echo "Update complete! Exporter is running."
