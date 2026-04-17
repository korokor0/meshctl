#!/usr/bin/env bash
# bootstrap-node.sh — Initialize a fat node for meshctl-agent.
#
# Creates directory structure, generates WireGuard private key,
# and optionally generates or installs a PSK master key.
#
# Usage:
#   ./bootstrap-node.sh                          # basic setup
#   ./bootstrap-node.sh --psk-generate            # also generate PSK master
#   ./bootstrap-node.sh --psk-install /path/to/master.key  # copy existing PSK master
#
# Run on the target node itself. Will request sudo if not root.
set -euo pipefail

PSK_MODE=""      # "", "generate", or "install"
PSK_SOURCE=""    # path to existing PSK master (for install mode)

while [[ $# -gt 0 ]]; do
    case "$1" in
        --psk-generate)
            PSK_MODE="generate"; shift ;;
        --psk-install)
            PSK_MODE="install"
            PSK_SOURCE="${2:?--psk-install requires a file path}"
            shift 2 ;;
        -h|--help)
            echo "Usage: $0 [--psk-generate | --psk-install /path/to/master.key]"
            echo ""
            echo "Creates /etc/meshctl/ directory structure and generates WireGuard keys."
            echo ""
            echo "Options:"
            echo "  --psk-generate         Generate a new PSK master key"
            echo "  --psk-install PATH     Install an existing PSK master key from PATH"
            exit 0 ;;
        *)
            echo "Unknown option: $1" >&2
            exit 1 ;;
    esac
done

# --- Require root (or sudo) ---

if [[ $EUID -ne 0 ]]; then
    if command -v sudo &>/dev/null; then
        echo "This script needs root privileges. Re-running with sudo..."
        exec sudo "$0" "$@"
    else
        echo "Error: this script must run as root (sudo not found, please run as root directly)." >&2
        exit 1
    fi
fi

# --- Check dependencies ---

if ! command -v wg &>/dev/null; then
    echo "Error: wg (wireguard-tools) not found. Install wireguard-tools first." >&2
    exit 1
fi

# --- Create directory structure ---

echo "Creating /etc/meshctl/ directory structure..."
mkdir -p /etc/meshctl/cache
chmod 700 /etc/meshctl

echo "  /etc/meshctl/          (mode 700)"
echo "  /etc/meshctl/cache/    (config cache)"

# --- Generate WireGuard private key ---

WG_KEY="/etc/meshctl/wireguard.key"

if [[ -f "$WG_KEY" ]]; then
    echo ""
    echo "WireGuard private key already exists at $WG_KEY — skipping."
    PUBKEY=$(wg pubkey < "$WG_KEY")
else
    echo ""
    echo "Generating WireGuard keypair..."
    install -m 0600 /dev/null "$WG_KEY"
    wg genkey > "$WG_KEY"
    PUBKEY=$(wg pubkey < "$WG_KEY")
    echo "  Private key written to: $WG_KEY (mode 0600)"
fi

echo ""
echo "  ┌──────────────────────────────────────────────────────┐"
echo "  │ Public key (add to meshctl.yaml under node's pubkey) │"
echo "  │                                                      │"
echo "  │  $PUBKEY  │"
echo "  └──────────────────────────────────────────────────────┘"

# --- PSK master key ---

PSK_KEY="/etc/meshctl/psk-master.key"

case "$PSK_MODE" in
    generate)
        if [[ -f "$PSK_KEY" ]]; then
            echo ""
            echo "PSK master already exists at $PSK_KEY — skipping."
            echo "To overwrite, remove the file first and re-run."
        else
            echo ""
            echo "Generating PSK master key..."
            install -m 0600 /dev/null "$PSK_KEY"
            wg genpsk > "$PSK_KEY"
            echo "  PSK master written to: $PSK_KEY (mode 0600)"
            echo ""
            echo "  IMPORTANT: Copy this file to every other fat node in the mesh."
            echo "  Use scp or another secure out-of-band method. NEVER commit it to git."
        fi
        ;;
    install)
        if [[ ! -f "$PSK_SOURCE" ]]; then
            echo "Error: PSK source file not found: $PSK_SOURCE" >&2
            exit 1
        fi
        echo ""
        echo "Installing PSK master from $PSK_SOURCE..."
        install -m 0600 "$PSK_SOURCE" "$PSK_KEY"
        echo "  PSK master installed to: $PSK_KEY (mode 0600)"
        ;;
    "")
        # No PSK requested — skip silently.
        ;;
esac

# --- Summary ---

echo ""
echo "Done. Directory layout:"
echo ""
ls -la /etc/meshctl/
echo ""
echo "Next steps:"
echo "  1. Add the public key above to meshctl.yaml"
echo "  2. Deploy agent binary to /usr/local/bin/meshctl-agent"
echo "  3. Create /etc/meshctl/agent.yaml (see examples/agent.example.yaml)"
echo "  4. Enable the service: systemctl enable --now meshctl-agent"
