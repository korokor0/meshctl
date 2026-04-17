#!/usr/bin/env bash
# install.sh — Remote deployment and upgrade of meshctl-agent.
#
# Usage:
#   ./scripts/install.sh [OPTIONS] <ssh-target>              # full install
#   ./scripts/install.sh upgrade [OPTIONS] <ssh-target...>   # binary-only upgrade (1 or more)
#
# Environment variables (or use flags):
#   MESHCTL_REPO_URL    Config repo git URL       (or --repo-url)
#   MESHCTL_DEPLOY_KEY  Path to deploy key        (or --deploy-key)
#   MESHCTL_PSK_MASTER  Path to PSK master key    (or --psk, optional)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$SCRIPT_DIR/../bin"

# --- Defaults ---
MODE="install"   # "install" or "upgrade"
NO_RESTART=false
NODE_NAME=""
SSH_TARGET=""
REPO_URL="${MESHCTL_REPO_URL:-}"
DEPLOY_KEY="${MESHCTL_DEPLOY_KEY:-}"
PSK_MASTER="${MESHCTL_PSK_MASTER:-}"
ARCH_OVERRIDE=""
REPO_BRANCH="main"
VERBOSE=false
PARALLEL=6
ROLLING=false

usage() {
    cat <<'EOF'
Usage:
  install.sh [OPTIONS] <ssh-target>              Full install (first time)
  install.sh upgrade [OPTIONS] <ssh-target...>   Update binary + restart (1 or more targets)

  <ssh-target>    SSH host — an ~/.ssh/config alias or user@host

Options:
  --node NAME         Node name in meshctl.yaml (default: ssh-target hostname)
  --repo-url URL      Config repo git URL (or set MESHCTL_REPO_URL)
  --deploy-key PATH   SSH deploy key file (or set MESHCTL_DEPLOY_KEY)
  --branch NAME       Config repo branch (default: main)
  --psk PATH          PSK master key to install (or set MESHCTL_PSK_MASTER)
  --arch amd64|arm64  Override remote architecture detection
  --no-restart        (upgrade) Replace binary without restarting the service
  --parallel N        (upgrade, multi-target) Max concurrent upgrades (default: 4)
  --rolling           (upgrade, multi-target) One at a time, abort on first failure
  -v, --verbose       Show detailed SSH output
  -h, --help          Show this help

Examples:
  ./scripts/install.sh HKG                              # full install
  ./scripts/install.sh upgrade HKG                      # upgrade single node
  ./scripts/install.sh upgrade HKG TYO LAX              # parallel upgrade 3 nodes
  ./scripts/install.sh upgrade --rolling HKG TYO LAX    # rolling upgrade
  ./scripts/install.sh upgrade --no-restart HKG         # upgrade binary only
EOF
    exit "${1:-0}"
}

# --- Check for subcommand ---
if [[ ${1:-} == "upgrade" ]]; then
    MODE="upgrade"
    shift
fi

# --- Parse arguments ---

POSITIONAL=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --node)       NODE_NAME="$2";   shift 2 ;;
        --repo-url)   REPO_URL="$2";    shift 2 ;;
        --deploy-key) DEPLOY_KEY="$2";  shift 2 ;;
        --branch)     REPO_BRANCH="$2"; shift 2 ;;
        --psk)        PSK_MASTER="$2";  shift 2 ;;
        --arch)       ARCH_OVERRIDE="$2"; shift 2 ;;
        --no-restart) NO_RESTART=true; shift ;;
        --parallel)   PARALLEL="$2"; shift 2 ;;
        --rolling)    ROLLING=true; shift ;;
        -v|--verbose) VERBOSE=true; shift ;;
        -h|--help)    usage 0 ;;
        -*)           echo "Unknown option: $1" >&2; usage 1 ;;
        *)            POSITIONAL+=("$1"); shift ;;
    esac
done

if [[ ${#POSITIONAL[@]} -lt 1 ]]; then
    echo "Error: at least one ssh-target required." >&2
    usage 1
fi

SSH_TARGET="${POSITIONAL[0]}"

# --- Derive node name (used for install + single upgrade) ---

derive_node_name() {
    local target="$1"
    local name="${target##*@}"
    name="${name%%:*}"
    name="${name%%]*}"
    name="${name##*[}"
    echo "${name,,}"
}

if [[ -z "$NODE_NAME" ]]; then
    NODE_NAME="$(derive_node_name "$SSH_TARGET")"
fi

# --- SSH/SCP flags ---

SSH_OPTS=()
SCP_OPTS=(-q)
if $VERBOSE; then
    SSH_OPTS+=(-v)
    SCP_OPTS=()
fi

# --- Detect remote architecture + find binary ---

detect_arch() {
    echo "==> Connecting to $SSH_TARGET ..."
    if [[ -n "$ARCH_OVERRIDE" ]]; then
        REMOTE_ARCH="$ARCH_OVERRIDE"
    else
        REMOTE_UNAME=$(ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "uname -m")
        case "$REMOTE_UNAME" in
            x86_64|amd64)   REMOTE_ARCH="amd64" ;;
            aarch64|arm64)  REMOTE_ARCH="arm64" ;;
            *)
                echo "Error: unsupported architecture: $REMOTE_UNAME" >&2
                exit 1 ;;
        esac
    fi

    AGENT_BINARY="$BIN_DIR/meshctl-agent-linux-${REMOTE_ARCH}"
    if [[ ! -f "$AGENT_BINARY" ]]; then
        echo "Error: binary not found: $AGENT_BINARY" >&2
        echo "  Run 'make release' first." >&2
        exit 1
    fi
    echo "    Remote arch: $REMOTE_ARCH"
}

# =============================================
#  UPGRADE — binary only, no config/key changes
# =============================================

do_upgrade_single() {
    detect_arch
    echo ""
    echo "==> Uploading meshctl-agent ($REMOTE_ARCH) + service unit ..."
    scp "${SCP_OPTS[@]}" "$AGENT_BINARY" "$SSH_TARGET:/tmp/meshctl-agent"
    scp "${SCP_OPTS[@]}" "$SCRIPT_DIR/../deployments/meshctl-agent.service" "$SSH_TARGET:/tmp/meshctl-agent.service"

    UPGRADE_SCRIPT=$(mktemp)
    trap 'rm -f "$UPGRADE_SCRIPT"' EXIT

    if $NO_RESTART; then
        echo "==> Replacing binary + service unit (no restart) ..."
        cat > "$UPGRADE_SCRIPT" <<'REMOTESCRIPT'
#!/usr/bin/env bash
set -euo pipefail
if [[ $EUID -ne 0 ]] && command -v sudo &>/dev/null; then SUDO="sudo"; else SUDO=""; fi
$SUDO install -m 0755 /tmp/meshctl-agent /usr/local/bin/meshctl-agent
$SUDO install -m 0644 /tmp/meshctl-agent.service /etc/systemd/system/meshctl-agent.service
$SUDO systemctl daemon-reload
rm -f /tmp/meshctl-agent /tmp/meshctl-agent.service /tmp/meshctl-upgrade.sh
echo "meshctl-agent binary + service unit replaced (service NOT restarted)."
REMOTESCRIPT
    else
        echo "==> Replacing binary + service unit + restarting ..."
        cat > "$UPGRADE_SCRIPT" <<'REMOTESCRIPT'
#!/usr/bin/env bash
set -euo pipefail
if [[ $EUID -ne 0 ]] && command -v sudo &>/dev/null; then SUDO="sudo"; else SUDO=""; fi
$SUDO install -m 0755 /tmp/meshctl-agent /usr/local/bin/meshctl-agent
$SUDO install -m 0644 /tmp/meshctl-agent.service /etc/systemd/system/meshctl-agent.service
$SUDO systemctl daemon-reload
rm -f /tmp/meshctl-agent /tmp/meshctl-agent.service /tmp/meshctl-upgrade.sh
$SUDO systemctl restart meshctl-agent
echo "meshctl-agent upgraded and restarted."
REMOTESCRIPT
    fi

    scp "${SCP_OPTS[@]}" "$UPGRADE_SCRIPT" "$SSH_TARGET:/tmp/meshctl-upgrade.sh"
    # Use -tt for interactive sudo when we have a terminal; skip in parallel mode.
    local ssh_tty=()
    if [[ -t 0 ]]; then
        ssh_tty=(-tt)
    fi
    ssh "${ssh_tty[@]}" "${SSH_OPTS[@]}" "$SSH_TARGET" "bash /tmp/meshctl-upgrade.sh"

    echo ""
    if $NO_RESTART; then
        echo "Done. Binary replaced. Restart manually when ready:"
        echo "  ssh $SSH_TARGET sudo systemctl restart meshctl-agent"
    else
        echo "Done. Verify:"
        echo "  ssh $SSH_TARGET sudo systemctl status meshctl-agent"
    fi
}

# do_upgrade handles single or multi-target upgrade.
do_upgrade() {
    local targets=("${POSITIONAL[@]}")

    # Single target: simple upgrade.
    if [[ ${#targets[@]} -eq 1 ]]; then
        do_upgrade_single
        return
    fi

    # Multiple targets: batch upgrade.
    local total=${#targets[@]}
    echo "==> Upgrading $total targets"
    if $ROLLING; then
        echo "    Mode: rolling (sequential, abort on failure)"
    else
        echo "    Mode: parallel (max $PARALLEL concurrent)"
        echo ""
        echo "    Checking passwordless sudo on all targets ..."
        local sudo_fail=false
        for target in "${targets[@]}"; do
            if ! ssh "${SSH_OPTS[@]}" "$target" "sudo -n true" 2>/dev/null; then
                echo "    [FAIL] $target — sudo requires a password" >&2
                sudo_fail=true
            fi
        done
        if $sudo_fail; then
            echo "" >&2
            echo "Error: parallel upgrade requires passwordless sudo (NOPASSWD)." >&2
            echo "  Configure /etc/sudoers on each node, or use --rolling for interactive mode." >&2
            return 1
        fi
        echo "    All targets OK."
    fi
    echo ""

    local failed=()
    local succeeded=()

    if $ROLLING; then
        for target in "${targets[@]}"; do
            echo "--- [$((${#succeeded[@]}+${#failed[@]}+1))/$total] $target ---"
            SSH_TARGET="$target"
            NODE_NAME="$(derive_node_name "$target")"
            if do_upgrade_single 2>&1; then
                succeeded+=("$target")
            else
                failed+=("$target")
                echo ""
                echo "Error: upgrade failed for $target — aborting rolling upgrade." >&2
                break
            fi
            echo ""
        done
    else
        local pids=()
        local running=0
        local log_dir
        log_dir=$(mktemp -d)
        trap "rm -rf '$log_dir'" EXIT

        for target in "${targets[@]}"; do
            while [[ $running -ge $PARALLEL ]]; do
                wait -n 2>/dev/null || true
                running=$((running - 1))
            done

            (
                SSH_TARGET="$target"
                NODE_NAME="$(derive_node_name "$target")"
                if do_upgrade_single; then
                    echo "OK" > "$log_dir/$target.status"
                else
                    echo "FAIL" > "$log_dir/$target.status"
                fi
            ) > "$log_dir/$target.log" 2>&1 &
            pids+=($!)
            running=$((running + 1))
        done

        for pid in "${pids[@]}"; do
            wait "$pid" 2>/dev/null || true
        done

        for target in "${targets[@]}"; do
            local status_file="$log_dir/$target.status"
            if [[ -f "$status_file" ]] && [[ "$(cat "$status_file")" == "OK" ]]; then
                succeeded+=("$target")
                echo "[OK]   $target"
            else
                failed+=("$target")
                echo "[FAIL] $target"
                if [[ -f "$log_dir/$target.log" ]]; then
                    tail -5 "$log_dir/$target.log" | sed 's/^/       /'
                fi
            fi
        done
    fi

    echo ""
    echo "====================================="
    echo "  Upgrade complete"
    echo "  Succeeded: ${#succeeded[@]}/$total"
    if [[ ${#failed[@]} -gt 0 ]]; then
        echo "  Failed:    ${#failed[@]} — ${failed[*]}"
    fi
    echo "====================================="

    if [[ ${#failed[@]} -gt 0 ]]; then
        return 1
    fi
}

# =============================================
#  INSTALL — full first-time setup
# =============================================

do_install() {
    if [[ ${#POSITIONAL[@]} -ne 1 ]]; then
        echo "Error: install requires exactly one ssh-target." >&2
        usage 1
    fi

    # --- Validate required parameters ---

    if [[ -z "$REPO_URL" ]]; then
        echo "Error: config repo URL required." >&2
        echo "  Set MESHCTL_REPO_URL or pass --repo-url" >&2
        exit 1
    fi

    if [[ -z "$DEPLOY_KEY" ]]; then
        echo "Error: deploy key path required." >&2
        echo "  Set MESHCTL_DEPLOY_KEY or pass --deploy-key" >&2
        exit 1
    fi

    if [[ ! -f "$DEPLOY_KEY" ]]; then
        echo "Error: deploy key not found: $DEPLOY_KEY" >&2
        exit 1
    fi

    if [[ -n "$PSK_MASTER" && ! -f "$PSK_MASTER" ]]; then
        echo "Error: PSK master key not found: $PSK_MASTER" >&2
        exit 1
    fi

    detect_arch
    echo "    Node name:   $NODE_NAME"
    echo "    Config repo: $REPO_URL"
    echo ""

    # --- Upload files to /tmp/ ---

    echo "==> Uploading files ..."
    scp "${SCP_OPTS[@]}" "$AGENT_BINARY" "$SSH_TARGET:/tmp/meshctl-agent"
    scp "${SCP_OPTS[@]}" "$DEPLOY_KEY" "$SSH_TARGET:/tmp/meshctl-deploy-key"
    scp "${SCP_OPTS[@]}" "$SCRIPT_DIR/../deployments/meshctl-agent.service" "$SSH_TARGET:/tmp/meshctl-agent.service"

    if [[ -n "$PSK_MASTER" ]]; then
        scp "${SCP_OPTS[@]}" "$PSK_MASTER" "$SSH_TARGET:/tmp/meshctl-psk-master"
    fi

    # --- Generate agent.yaml locally, upload ---

    AGENT_YAML=$(mktemp)
    trap 'rm -f "$AGENT_YAML"' EXIT

    cat > "$AGENT_YAML" <<EOF
node_name: ${NODE_NAME}

repo:
  sources:
    - type: git
      url: "${REPO_URL}"
      branch: ${REPO_BRANCH}
      ssh_key: "/etc/meshctl/deploy_key"
    - type: local
      path: "/etc/meshctl/manual-configs/"

  fetch_timeout: 30s
  local_cache: "/etc/meshctl/cache/"

config_sync_interval: 5m
probe_interval: 30s
probe_port: 9473
bird_socket: "/var/run/bird/bird.ctl"
bird_include_path: "/etc/bird/meshctl.conf"

private_key_file: "/etc/meshctl/wireguard.key"
EOF

    if [[ -n "$PSK_MASTER" ]]; then
        echo 'psk_master_file: "/etc/meshctl/psk-master.key"' >> "$AGENT_YAML"
    fi

    scp "${SCP_OPTS[@]}" "$AGENT_YAML" "$SSH_TARGET:/tmp/meshctl-agent.yaml"

    # --- Generate remote setup script ---

    REMOTE_SCRIPT=$(mktemp)
    trap 'rm -f "$AGENT_YAML" "$REMOTE_SCRIPT"' EXIT

    cat > "$REMOTE_SCRIPT" <<'REMOTESCRIPT'
#!/usr/bin/env bash
set -euo pipefail

# Use sudo if available and not already root; otherwise run directly.
if [[ $EUID -ne 0 ]] && command -v sudo &>/dev/null; then
    SUDO="sudo"
else
    SUDO=""
fi

# 1. Install binary
$SUDO install -m 0755 /tmp/meshctl-agent /usr/local/bin/meshctl-agent

# 2. Create directory structure
$SUDO mkdir -p /etc/meshctl/cache
$SUDO chmod 700 /etc/meshctl

# Create placeholder meshctl BIRD includes (agent will overwrite on first sync)
$SUDO mkdir -p /etc/bird
for f in /etc/bird/meshctl.conf /etc/bird/meshctl-underlay.conf; do
    [[ -f "$f" ]] || $SUDO touch "$f"
done

# 3. Generate WireGuard key (skip if exists)
WG_KEY="/etc/meshctl/wireguard.key"
if [[ ! -f "$WG_KEY" ]]; then
    $SUDO bash -c "install -m 0600 /dev/null '$WG_KEY' && wg genkey > '$WG_KEY'"
fi

# 4. Install deploy key
$SUDO install -m 0600 /tmp/meshctl-deploy-key /etc/meshctl/deploy_key

# 5. Install PSK master (if uploaded)
if [[ -f /tmp/meshctl-psk-master ]]; then
    $SUDO install -m 0600 /tmp/meshctl-psk-master /etc/meshctl/psk-master.key
fi

# 6. Install agent.yaml
$SUDO install -m 0600 /tmp/meshctl-agent.yaml /etc/meshctl/agent.yaml

# 7. Install systemd unit
$SUDO install -m 0644 /tmp/meshctl-agent.service /etc/systemd/system/meshctl-agent.service
$SUDO systemctl daemon-reload
$SUDO systemctl enable --now meshctl-agent

# 8. Clean up
rm -f /tmp/meshctl-agent /tmp/meshctl-deploy-key /tmp/meshctl-psk-master \
      /tmp/meshctl-agent.yaml /tmp/meshctl-agent.service /tmp/meshctl-setup.sh

# 9. Write public key for local script
$SUDO cat "$WG_KEY" | wg pubkey > /tmp/meshctl-pubkey

# 10. Detect public IPv4 and IPv6 addresses
detect_ip() {
    local family="$1"  # -4 or -6
    local ip=""
    # Try external services first (most reliable for public IP)
    if command -v curl &>/dev/null; then
        for svc in "https://ifconfig.co" "https://icanhazip.com" "https://api.ipify.org"; do
            ip=$(curl -s --max-time 3 "$family" "$svc" 2>/dev/null | tr -d '[:space:]')
            if [[ -n "$ip" ]]; then echo "$ip"; return; fi
        done
    fi
    # Fallback: parse default route interface
    local dev
    if [[ "$family" == "-4" ]]; then
        dev=$(ip -4 route show default 2>/dev/null | head -1 | grep -oP 'dev \K\S+')
        if [[ -n "$dev" ]]; then
            ip=$(ip -4 addr show dev "$dev" 2>/dev/null | grep -oP 'inet \K[0-9.]+' | head -1)
            if [[ -n "$ip" ]]; then echo "$ip"; return; fi
        fi
    else
        dev=$(ip -6 route show default 2>/dev/null | head -1 | grep -oP 'dev \K\S+')
        if [[ -n "$dev" ]]; then
            ip=$(ip -6 addr show dev "$dev" scope global 2>/dev/null | grep -oP 'inet6 \K[0-9a-f:]+' | head -1)
            if [[ -n "$ip" ]]; then echo "$ip"; return; fi
        fi
    fi
}

{
    echo "ipv4=$(detect_ip -4)"
    echo "ipv6=$(detect_ip -6)"
} > /tmp/meshctl-endpoints
REMOTESCRIPT

    scp "${SCP_OPTS[@]}" "$REMOTE_SCRIPT" "$SSH_TARGET:/tmp/meshctl-setup.sh"

    # --- Run setup ---

    echo "==> Installing ..."
    ssh -tt "${SSH_OPTS[@]}" "$SSH_TARGET" "bash /tmp/meshctl-setup.sh"
    SSH_EXIT=$?

    if [[ $SSH_EXIT -ne 0 ]]; then
        echo "Error: remote setup failed (exit $SSH_EXIT)" >&2
        exit $SSH_EXIT
    fi

    # Read pubkey and detected endpoints
    PUBKEY=$(ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "cat /tmp/meshctl-pubkey 2>/dev/null && rm -f /tmp/meshctl-pubkey" | tr -d '\r\n')
    ENDPOINTS=$(ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "cat /tmp/meshctl-endpoints 2>/dev/null && rm -f /tmp/meshctl-endpoints")
    DETECTED_V4=$(echo "$ENDPOINTS" | grep '^ipv4=' | cut -d= -f2 | tr -d '[:space:]')
    DETECTED_V6=$(echo "$ENDPOINTS" | grep '^ipv6=' | cut -d= -f2 | tr -d '[:space:]')

    if [[ -z "$PUBKEY" ]]; then
        echo "Warning: could not read public key. Check manually:" >&2
        echo "  ssh $SSH_TARGET 'sudo cat /etc/meshctl/wireguard.key | wg pubkey'" >&2
    fi

    echo ""
    echo "====================================="
    echo "  meshctl-agent deployed to $SSH_TARGET"
    echo "  Node name: $NODE_NAME"
    if [[ -n "$DETECTED_V4" ]]; then echo "  IPv4: $DETECTED_V4"; fi
    if [[ -n "$DETECTED_V6" ]]; then echo "  IPv6: $DETECTED_V6"; fi
    echo "====================================="
    echo ""
    echo "Add to meshctl.yaml:"
    echo ""
    echo "  - name: $NODE_NAME"
    echo "    type: linux"
    echo "    endpoint:"
    if [[ -n "$DETECTED_V4" ]]; then
        echo "      ipv4: \"$DETECTED_V4\""
    fi
    if [[ -n "$DETECTED_V6" ]]; then
        echo "      ipv6: \"$DETECTED_V6\""
    fi
    if [[ -z "$DETECTED_V4" && -z "$DETECTED_V6" ]]; then
        echo "      ipv4: \"\"   # detection failed — fill in manually"
    fi
    echo "    pubkey: \"$PUBKEY\""
    echo "    underlay: {}"
    echo ""
    if [[ -z "$DETECTED_V4" || -z "$DETECTED_V6" ]]; then
        echo "Note: some IPs could not be detected. Verify and fill in manually."
        echo ""
    fi
    echo "Verify:"
    echo "  ssh $SSH_TARGET systemctl status meshctl-agent"
    echo "  ssh $SSH_TARGET journalctl -u meshctl-agent -f"
}

# --- Dispatch ---

case "$MODE" in
    install) do_install ;;
    upgrade) do_upgrade ;;
esac
