#!/bin/sh
set -eu

CONFIG_PATH="${BESTIP_CONFIG:-/data/config.json}"
if [ ! -f "$CONFIG_PATH" ]; then
  mkdir -p "$(dirname "$CONFIG_PATH")"
  cp /app/config.example.json "$CONFIG_PATH"
  chmod 600 "$CONFIG_PATH" || true
  echo "Created initial config at $CONFIG_PATH"
fi

if ! command -v cfst >/dev/null 2>&1; then
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) pkg=cfst_linux_amd64.tar.gz ;;
    aarch64|arm64) pkg=cfst_linux_arm64.tar.gz ;;
    armv7l) pkg=cfst_linux_armv7.tar.gz ;;
    *) echo "Unsupported arch: $arch" >&2; exit 1 ;;
  esac
  echo "Downloading latest CFST: $pkg"
  cd /tmp
  curl -fL "https://github.com/XIU2/CloudflareSpeedTest/releases/latest/download/$pkg" -o cfst.tar.gz
  tar -xzf cfst.tar.gz
  bin="$(find . -maxdepth 2 -type f -name cfst | head -n1)"
  test -n "$bin"
  install -m 0755 "$bin" /usr/local/bin/cfst
fi
exec /usr/local/bin/bestip-manager
