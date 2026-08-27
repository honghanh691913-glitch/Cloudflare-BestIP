#!/bin/sh
set -eu

# Update helper must start immediately: it does not need config or CFST.
if [ "${BESTIP_UPDATE_HELPER:-}" = "1" ]; then
  exec /usr/local/bin/bestip-manager
fi

CONFIG_PATH="${BESTIP_CONFIG:-/data/config.json}"

if [ ! -f "$CONFIG_PATH" ]; then
  mkdir -p "$(dirname "$CONFIG_PATH")"
  cp /app/config.example.json "$CONFIG_PATH"
  chmod 600 "$CONFIG_PATH" || true
  echo "Created initial config at $CONFIG_PATH"
fi

mkdir -p /data/bin

download_cfst() {
  if [ -x /data/bin/cfst ]; then
    echo "CFST already available: /data/bin/cfst"
    return 0
  fi

  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) pkg="cfst_linux_amd64.tar.gz" ;;
    aarch64|arm64) pkg="cfst_linux_arm64.tar.gz" ;;
    armv7l) pkg="cfst_linux_armv7.tar.gz" ;;
    *) echo "Unsupported architecture for CFST: $arch"; return 0 ;;
  esac

  (
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    while [ ! -x /data/bin/cfst ]; do
      echo "Downloading CFST in background: $pkg"
      if curl -fL \
        --retry 5 \
        --retry-delay 3 \
        --retry-all-errors \
        --connect-timeout 20 \
        "https://github.com/XIU2/CloudflareSpeedTest/releases/latest/download/$pkg" \
        -o "$tmpdir/cfst.tar.gz"; then

        rm -rf "$tmpdir/unpack"
        mkdir -p "$tmpdir/unpack"

        if tar -xzf "$tmpdir/cfst.tar.gz" -C "$tmpdir/unpack"; then
          bin="$(find "$tmpdir/unpack" -maxdepth 3 -type f -name cfst | head -n 1)"
          if [ -n "$bin" ]; then
            cp "$bin" /data/bin/cfst
            chmod 0755 /data/bin/cfst
            echo "CFST ready: /data/bin/cfst"
            break
          fi
        fi
      fi

      echo "CFST download failed; retrying in 30 seconds..."
      sleep 30
    done
  ) &
}

download_cfst
exec /usr/local/bin/bestip-manager
