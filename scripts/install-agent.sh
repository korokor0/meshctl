#!/usr/bin/env bash
# Deploy meshctl-agent to a remote Linux fat node.
set -euo pipefail

usage() {
    echo "Usage: $0 --node NAME --host USER@HOST --binary PATH --repo-url URL --ssh-key PATH"
    exit 1
}

NODE="" HOST="" BINARY="" REPO_URL="" SSH_KEY=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --node)     NODE="$2"; shift 2 ;;
        --host)     HOST="$2"; shift 2 ;;
        --binary)   BINARY="$2"; shift 2 ;;
        --repo-url) REPO_URL="$2"; shift 2 ;;
        --ssh-key)  SSH_KEY="$2"; shift 2 ;;
        *) usage ;;
    esac
done

[[ -z "$NODE" || -z "$HOST" || -z "$BINARY" || -z "$REPO_URL" || -z "$SSH_KEY" ]] && usage

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Deploying meshctl-agent to $HOST (node: $NODE)..."

# Upload binary.
scp "$BINARY" "$HOST:/usr/local/bin/meshctl-agent"
ssh "$HOST" "chmod +x /usr/local/bin/meshctl-agent"

# Upload systemd unit.
scp "$SCRIPT_DIR/../deployments/meshctl-agent.service" "$HOST:/etc/systemd/system/meshctl-agent.service"

# Upload deploy key.
ssh "$HOST" "mkdir -p /etc/meshctl/cache"
scp "$SSH_KEY" "$HOST:/etc/meshctl/deploy_key"
ssh "$HOST" "chmod 600 /etc/meshctl/deploy_key"

# Generate agent.yaml.
ssh "$HOST" "cat > /etc/meshctl/agent.yaml" << EOF
node_name: $NODE

repo:
  sources:
    - type: git
      url: "$REPO_URL"
      branch: main
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
EOF

# Enable and start.
ssh "$HOST" "systemctl daemon-reload && systemctl enable --now meshctl-agent"

echo "Done. Agent deployed to $HOST as node $NODE."
echo "Check status: ssh $HOST systemctl status meshctl-agent"
