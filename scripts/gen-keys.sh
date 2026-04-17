#!/usr/bin/env bash
# Generate a WireGuard keypair for a fat node, or a PSK master secret.
#
# Usage:
#   gen-keys.sh wg        # print a WG keypair (stdout)
#   gen-keys.sh wg-install /etc/meshctl/wireguard.key
#                         # write private key to file (0600), print pubkey
#   gen-keys.sh psk       # print a base64 PSK master secret
#   gen-keys.sh psk-install /etc/meshctl/psk-master.key
#                         # write PSK master to file (0600)
set -euo pipefail

if ! command -v wg &>/dev/null; then
    echo "error: wg (wireguard-tools) not found" >&2
    exit 1
fi

cmd=${1:-wg}

case "$cmd" in
    wg)
        privkey=$(wg genkey)
        pubkey=$(echo "$privkey" | wg pubkey)
        echo "Private key: $privkey"
        echo "Public key:  $pubkey"
        echo ""
        echo "Store the private key as /etc/meshctl/wireguard.key (mode 0600)."
        echo "Add the public key to meshctl.yaml under the node's 'pubkey' field."
        ;;
    wg-install)
        dest=${2:?dest path required}
        install -m 0600 /dev/null "$dest"
        wg genkey > "$dest"
        pubkey=$(wg pubkey < "$dest")
        echo "Private key written to: $dest"
        echo "Public key:  $pubkey"
        ;;
    psk)
        # 32 bytes of randomness, base64. Use wg genkey since it already
        # emits a clamped 32-byte base64 string — we're not using it as a
        # curve25519 key so clamping is irrelevant.
        wg genpsk
        ;;
    psk-install)
        dest=${2:?dest path required}
        install -m 0600 /dev/null "$dest"
        wg genpsk > "$dest"
        echo "PSK master written to: $dest"
        echo "Copy this file to every fat node in the mesh."
        ;;
    *)
        echo "unknown command: $cmd" >&2
        exit 2
        ;;
esac
