#!/usr/bin/env bash
# Scaffold a new mesh-configs repository.
set -euo pipefail

DEST="${1:?Usage: $0 <dest-dir>}"

if [ -d "$DEST" ]; then
    echo "error: $DEST already exists" >&2
    exit 1
fi

mkdir -p "$DEST"/{output,agents}

# Copy example inventory.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/../examples/meshctl.example.yaml" ]; then
    cp "$SCRIPT_DIR/../examples/meshctl.example.yaml" "$DEST/meshctl.yaml"
else
    cat > "$DEST/meshctl.yaml" << 'EOF'
# meshctl.yaml — edit this file with your node inventory
global:
  output_dir: "./output"

nodes: []

link_policy:
  mode: full
EOF
fi

# Initialize git repo.
cd "$DEST"
git init
git add -A
git commit -m "initial mesh-configs scaffold"

echo ""
echo "Config repo created at $DEST"
echo "Next steps:"
echo "  1. Edit meshctl.yaml with your node inventory"
echo "  2. Run: meshctl generate --config meshctl.yaml"
echo "  3. git add -A && git commit -m 'initial config'"
echo "  4. Push to your private git remote"
