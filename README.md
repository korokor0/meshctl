# meshctl

Automated overlay mesh network controller with dynamic OSPF cost tuning.

meshctl replaces manual per-pair WireGuard configuration (O(n²) labor) with a single declarative YAML inventory. It generates platform-specific configs for Linux, MikroTik RouterOS, and static nodes, and optionally runs a per-node agent that measures inter-node latency to dynamically adjust OSPF link costs.

## How it works

```
  Inventory YAML ──→ meshctl generate ──→ Per-node configs
                                              │
                          ┌───────────────────┼──────────────────┐
                          ▼                   ▼                  ▼
                     Linux (fat)         Linux (fat)        RouterOS
                     meshctl-agent       meshctl-agent      operator imports .rsc
                       │                   │
                       └── UDP probes ─────┘
                       (one-way delay measurement)
```

Two binaries, two repos:

| Binary | Purpose |
|---|---|
| `meshctl` | Config generator — reads YAML, outputs WireGuard + BIRD + RouterOS configs |
| `meshctl-agent` | Node agent — pulls config, applies WG/BIRD, probes peers, adjusts OSPF costs |

The **code repo** (this one) contains Go source. A separate **config repo** (private) holds `meshctl.yaml` and generated output.

## Quick start

### Build

```bash
make build
# Output: bin/meshctl, bin/meshctl-agent
```

### Generate configs

```bash
# Copy and edit the example inventory
cp examples/meshctl.example.yaml meshctl.yaml
vim meshctl.yaml

# Validate
bin/meshctl validate --config meshctl.yaml

# Preview topology
bin/meshctl show-mesh --config meshctl.yaml

# Generate all configs
bin/meshctl generate --config meshctl.yaml

# Review what changed
bin/meshctl diff --config meshctl.yaml
```

### Deploy agent to a Linux node

```bash
# One-command install (HKG is an ~/.ssh/config alias, or use root@1.1.1.1)
./scripts/install.sh HKG

# With explicit options
./scripts/install.sh --node hk-core --repo-url git@github.com:org/mesh-configs.git \
    --deploy-key ~/.ssh/meshctl_deploy_key HKG

# Or set env vars once, then just pass the SSH target
export MESHCTL_REPO_URL="git@github.com:yourorg/mesh-configs.git"
export MESHCTL_DEPLOY_KEY="~/.ssh/meshctl_deploy_key"
./scripts/install.sh HKG
```

The script auto-detects the remote architecture, uploads the correct binary from `bin/`, generates a WireGuard keypair on the node, and prints the public key for you to add to `meshctl.yaml`.

Run `make release` first to cross-compile the agent binaries.

### Upgrade agent binary

```bash
# Replace binary + restart service
./scripts/install.sh upgrade HKG

# Replace binary only, restart later manually
./scripts/install.sh upgrade --no-restart HKG

# Batch upgrade multiple nodes in parallel (max 6 concurrent)
./scripts/install.sh upgrade HKG TYO LAX

# Rolling upgrade — sequential, abort on first failure
./scripts/install.sh upgrade --rolling HKG TYO LAX
```

### Apply to RouterOS

```bash
scp output/hk-edge/full-setup.rsc admin@192.168.88.1:/
ssh admin@192.168.88.1 "/import full-setup.rsc"
```

## Node types

| Type | Config delivery | Probing | OSPF cost |
|---|---|---|---|
| `linux` (fat) | Agent pulls from git | UDP one-way delay | Dynamic (band state machine) |
| `routeros` (thin) | Operator imports .rsc | Configurable (see below) | Configurable |
| `static` | Reference snippets | Configurable (see below) | Configurable |

### Cost modes for thin/static peers

Fat nodes support two ways to determine OSPF cost toward thin/static peers, controlled by the per-node `cost_mode` field:

**`cost_mode: probe`** (default) — ICMP rtt/2 dynamic cost:
```yaml
  - name: friend-node
    type: static
    cost_mode: probe          # default, can be omitted
    static_cost: 200          # optional: fallback when ICMP fails (instead of penalty 65535)
```
The fat node sends ICMP echo, uses `rtt/2` as estimated forward delay, and feeds it into the cost band state machine. If `static_cost` is set and all probes fail, uses that value as fallback instead of the penalty cost (65535).

**`cost_mode: static`** — fixed cost, no probing:
```yaml
  - name: hk-edge
    type: routeros
    cost_mode: static
    static_cost: 150          # required when cost_mode is static
```
The fat node uses cost 150 unconditionally. No ICMP, no probing, no band transitions. The agent skips this peer entirely during probe rounds.

## Inventory format

```yaml
global:
  wg_listen_port: 51820
  probe_interval: 30s
  cost_bands:
    - { up: 0ms,   down: 0ms,   cost: 20,   hold: 5 }
    - { up: 4ms,   down: 2ms,   cost: 80,   hold: 5 }
    - { up: 12ms,  down: 8ms,   cost: 160,  hold: 5 }
    - { up: 30ms,  down: 20ms,  cost: 250,  hold: 5 }
    - { up: 60ms,  down: 40ms,  cost: 350,  hold: 5 }
    - { up: 100ms, down: 70ms,  cost: 480,  hold: 5 }
    - { up: 160ms, down: 120ms, cost: 640,  hold: 5 }
    - { up: 220ms, down: 180ms, cost: 840,  hold: 5 }
    - { up: 300ms, down: 260ms, cost: 1100, hold: 5 }
  # igp_table4: igptable4    # BIRD table for OSPF IPv4 routes
  # igp_table6: igptable6    # BIRD table for OSPF IPv6 routes
  # wg_iface_prefix: igp-    # WireGuard interface name prefix (default: "igp-")
  # bandwidth_threshold: 300 # Mbps — links below this get extra cost (default: 300)
  # reference_bandwidth: 3000 # Mbps — for Cisco-style auto-cost formula (default: 3000)

nodes:
  # node_id: deterministic addressing. V4LL = base + node_id, fe80 = fe80::127:<node_id>.
  # If omitted, auto-assigned alphabetically. Explicit IDs prevent address shifts.

  # Dual-stack: generator picks v6 or v4 endpoint per peer
  - name: hk-core
    type: linux
    node_id: 1                # → 169.254.0.1, fe80::127:1
    bandwidth: 10000          # Mbps. Omit to default to bandwidth_threshold (no penalty).
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    loopback: 10.200.255.1   # stable overlay address: BIRD router ID, BGP peering endpoint, announced as /32 stub into OSPF
    announce:
      - 192.168.1.0/24
    pubkey: "aB3d...="
    underlay: {}              # prefsrc defaults to endpoint IPs

  - name: hk-edge
    type: routeros
    node_id: 2                # → 169.254.0.2, fe80::127:2
    bandwidth: 100            # 100Mbps — below threshold, gets cost penalty
    endpoint:
      ipv6: "2001:db8::2"
    loopback: 10.200.255.2
    pubkey: "cD5f...="
    wg_listen_port: 13231
    wg_peer_port: 60001       # fixed port: all peers listen on 60001 for hk-edge
    cost_mode: static         # fixed cost, no probing
    static_cost: 150

  - name: friend-node
    type: static
    node_id: 10               # → 169.254.0.10, fe80::127:a
    endpoint:
      ipv4: "203.0.113.99"
    loopback: 10.200.255.10
    pubkey: "eF7g...="
    cost_mode: probe          # ICMP rtt/2, with static fallback
    static_cost: 200

link_policy:
  mode: full
```

See `examples/meshctl.example.yaml` for a complete example.

### Nodes behind NAT (no public IP)

Nodes without a public IP can join the mesh. Omit the `endpoint` section entirely:

```yaml
  - name: home-node
    type: linux
    loopback: 10.200.255.20
    pubkey: "KEY="
    peers_with:
      - hk-core
      - jp-relay
```

The NAT node initiates WireGuard connections to its peers. Each link must have at least one side with a public endpoint — `meshctl generate` will error otherwise. Use `peers_with` to specify which public nodes to connect to. `persistent_keepalive` keeps tunnels alive through NAT.

## Dual-stack endpoints

Nodes can have both IPv4 and IPv6 addresses. Declare them directly in the `endpoint` struct:

```yaml
  - name: hk-core
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
```

The generator picks the best endpoint per peer — prefers IPv6 when both sides support it, falls back to IPv4 otherwise. IPv4-only peers automatically get the IPv4 endpoint.

### Per-peer listen ports

Each WireGuard peer gets its own interface (e.g. `igp-jp-relay`), each needing a unique listen port. Ports are auto-assigned from the node's base port (`wg_listen_port` or `global.wg_listen_port`) in alphabetical peer order.

To prevent port shifts when adding/removing nodes, set `wg_peer_port` on a node. This fixes the port that **all peers** use to listen for that node:

```yaml
  - name: jp-relay
    wg_peer_port: 60001    # every peer's igp-jp-relay interface listens on 60001
```

Auto-assigned ports skip any port already claimed by a `wg_peer_port`. Two nodes cannot share the same `wg_peer_port` value.

For domain-based endpoints or DDNS, use the `domain` and `ddns` fields:

```yaml
  - name: us-west
    endpoint:
      domain: "us-west.example.com"
      ipv4: "203.0.113.50"    # explicit IPs for underlay route generation
      ipv6: "2001:db8:cafe::1"
      ddns: false             # reserved for future re-resolution
```

## Underlay static routes

On multi-homed hosts, the kernel may pick the wrong source IP when sending WireGuard UDP packets, causing return traffic to arrive on the wrong interface or be dropped by RPF. The `underlay` config generates BIRD static routes with `krt_prefsrc` to pin the source address.

```yaml
nodes:
  - name: hk-core
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    underlay: {}              # prefsrc defaults to endpoint IPs (1.2.3.4 and 2001:db8::1)

  # Override prefsrc when you want a different source than the endpoint
  - name: jp-relay
    endpoint:
      ipv4: "198.51.100.5"
    underlay:
      prefsrc4: "ens3"        # interface name — agent picks primary IP
      # prefsrc also accepts "auto" — agent detects from default route
```

`prefsrc` defaults to the node's own endpoint IP for each address family. Override only when needed (e.g. interface name or `"auto"`). When `prefsrc` is an interface name, the agent follows Linux kernel source address selection: primary address first, skip deprecated addresses (`preferred_lft 0sec`).

The agent auto-detects the default gateway at runtime via `ip route get` and writes `/etc/bird/meshctl-underlay.conf` with `protocol static` blocks. These routes target `master4`/`master6` for direct kernel FIB installation and are not redistributed into OSPF.

For domain endpoints, provide explicit IPs in the `endpoint` struct for underlay route generation:

```yaml
  - name: us-west
    endpoint:
      domain: "us-west.example.com"
      ipv4: "203.0.113.50"
      ipv6: "2001:db8:cafe::1"
      ddns: false               # reserved for future re-resolution
    underlay: {}                # prefsrc defaults to endpoint IPs
```

## BIRD integration

OSPF routes go into dedicated tables (`igptable4`/`igptable6` by default, configurable via `global.igp_table4`/`igp_table6`). The operator pipes them to master in `bird.conf`:

```
# /etc/bird/bird.conf (operator-managed)
router id 10.200.255.1;
protocol device {}
protocol direct { ipv4; ipv6; }
protocol kernel k4 { ipv4 { export all; }; }
protocol kernel k6 { ipv6 { export all; }; }
protocol static lo4 { ipv4; route 10.200.255.1/32 via "lo"; }

# Pipe IGP tables to master
protocol pipe igp4 { table igptable4; peer table master4; import none; export all; }
protocol pipe igp6 { table igptable6; peer table master6; import none; export all; }

include "/etc/bird/meshctl.conf";           # agent-managed (OSPF + table declarations)
include "/etc/bird/meshctl-underlay.conf";  # agent-managed (underlay static routes)
include "/etc/bird/bgp.conf";               # operator-managed
```

## Tunnel addressing

meshctl automatically selects the addressing mode per link:

- **Both Linux** — IPv6 link-local (`fe80::`) + OSPFv3 AF. Zero IP allocation needed.
- **Any non-Linux endpoint** — Deterministic `169.254.x.x/31` + OSPFv2. Address derived from hash of sorted node-pair names.

## Dynamic cost tuning

Fat nodes run a UDP probe protocol (port 9473, over WireGuard tunnels) that measures **one-way forward delay** using NTP-synced timestamps:

```
Node A                          Node B
  │── Request {seq, T1} ───────→│
  │                              │  T2 = recv time
  │                              │  T3 = send time
  │←── Reply {seq, T1, T2, T3} ─│
T4 = recv time

forward_delay (A→B) = T2 - T1
```

The forward delay feeds into a cost band state machine with hysteresis to prevent OSPF churn:

| One-way delay | OSPF cost | Hysteresis |
|---|---|---|
| < 4ms | 20 | — |
| 4–12ms | 50 | down at 2ms |
| 12–30ms | 100 | down at 8ms |
| 30–60ms | 200 | down at 20ms |
| > 60ms | 500 | down at 40ms |
| unreachable | 65535 (or `static_cost` fallback) | 3 consecutive failures |

Band transitions require 5 consecutive probes in the new band (hold count) before the cost changes.

Peers with `cost_mode: static` bypass this entirely and always use the configured `static_cost`.

### Bandwidth-aware cost

Links below `bandwidth_threshold` (default 300 Mbps) receive an additive OSPF cost penalty using the Cisco-style auto-cost formula:

```
link_bw = min(A.bandwidth, B.bandwidth)
penalty = reference_bandwidth / link_bw - reference_bandwidth / bandwidth_threshold
```

Nodes without an explicit `bandwidth` field default to `bandwidth_threshold` (no penalty). This ensures full backward compatibility — existing configs without bandwidth declarations behave identically.

Example (reference=1000, threshold=300): a 100 Mbps link gets +6 cost, a 50 Mbps link gets +16.

## Agent operation

`meshctl-agent` runs three independent loops:

1. **Config sync** (default 5m) — fetches config from git/http/local sources with fallback chain and local cache
2. **Probe** (default 30s) — UDP timestamp probes to fat peers, ICMP echo to thin/static peers (skips `cost_mode: static` peers)
3. **Cost adjust** (after each probe round) — updates BIRD include when a cost band changes

Config fetch failures never block probing or cost adjustment. The agent continues with cached config. Repeated fetch failures trigger exponential backoff (30s → 10min cap) to avoid hammering git servers.

```bash
meshctl-agent --config /etc/meshctl/agent.yaml
```

See `examples/agent.example.yaml` for agent configuration.

### Monitoring agent status

Enable the HTTP health endpoint in `agent.yaml`:

```yaml
health_addr: ":9474"
```

Then query with the built-in status command:

```bash
# Auto-reads health_addr from agent.yaml
meshctl-agent status

# Or specify the address directly
meshctl-agent status --addr :9474

# JSON output for scripting
meshctl-agent status --json
```

Example output:

```
Node:        hk-core
Uptime:      2h15m30s
Config age:  4m12s
Last probe:  28s ago
Peers:       3
NTP synced:  yes  (offset 1.2ms)

PEER         INTERFACE    FORWARD  RTT      BAND  COST         MODE    STATUS
hk-edge      igp-hk-edg   -        -        -     150          static  -
jp-relay     igp-jp-rel   3.2ms    6.1ms    0     20           probe   ok
friend-node  igp-friend   6.1ms    12.3ms   1     56 (+bw6)    icmp    ok
```

The HTTP endpoint also serves raw JSON:
- `GET /health` — agent health (uptime, config age, NTP status)
- `GET /peers` — per-peer latency, OSPF cost, band state

You can also verify OSPF costs directly via BIRD:

```bash
birdc show ospf interface    # current OSPF cost per interface
birdc show ospf neighbors    # neighbor state
```

## Project structure

```
cmd/meshctl/           CLI entry point (generate, validate, diff, show-mesh, psk)
cmd/meshctl-agent/     Agent entry point (run, status)
internal/config/       YAML inventory parsing and validation
internal/mesh/         Link enumeration, mode selection, /31 addressing
internal/generate/     Config generators (BIRD, RouterOS, static)
internal/cost/         Cost band state machine with EWMA + hysteresis
internal/probe/        UDP probe protocol and ICMP fallback
internal/bird/         BIRD control socket client
internal/agent/        Agent runtime (fetch, apply, probe loop)
examples/              Example configs
scripts/               Deployment helpers
deployments/           systemd unit
```

## Keys and SSH

meshctl uses several different keys. Here's what each one is:

| Key | What it is | Where it lives | Purpose |
|---|---|---|---|
| **WireGuard private key** | Curve25519 private key | `/etc/meshctl/wireguard.key` on each node | Encrypts WireGuard tunnels |
| **WireGuard public key** | Derived from private key | `meshctl.yaml` `pubkey` field | Peers use it to identify this node |
| **Deploy key** | Ed25519 SSH key | `/etc/meshctl/deploy_key` on each node | Agent uses it to `git fetch` config repo |
| **PSK master** (optional) | 32-byte symmetric secret | `/etc/meshctl/psk-master.key` on each node | Derives per-link PSKs for post-quantum protection |
| **Your SSH key** | Your personal SSH key | `~/.ssh/` on your laptop | You use it to SSH into nodes for setup |

### WireGuard keys

Private keys never live in `meshctl.yaml` — only the public key goes there. The `install.sh` script generates the keypair on the remote node automatically and prints the public key.

To generate manually on a node:

```bash
./scripts/gen-keys.sh wg-install /etc/meshctl/wireguard.key
# Copy the printed public key into meshctl.yaml under the node's `pubkey`.
```

Or use the bootstrap script for local setup:

```bash
./scripts/bootstrap-node.sh
```

### Deploy key

The deploy key is a **read-only SSH key** that lets the agent pull from your private config repo. It is NOT your personal SSH key — it's a dedicated key with minimal permissions.

```bash
# Generate once (on your laptop)
ssh-keygen -t ed25519 -f ~/.ssh/meshctl_deploy_key -N "" -C "meshctl-agent"

# Add the PUBLIC key to your config repo (GitHub → Settings → Deploy keys, read-only)
cat ~/.ssh/meshctl_deploy_key.pub

# The install.sh script uploads the PRIVATE key to each node automatically
./scripts/install.sh --deploy-key ~/.ssh/meshctl_deploy_key HKG
```

All fat nodes can share the same deploy key (all read the same repo).

```
Your laptop                       Remote node                     GitHub/GitLab
  │                                 │                               │
  │── ssh (your key) ──────────────>│                               │
  │   scp deploy_key ─────────────>│                               │
  │                                 │── git fetch (deploy_key) ─────>│
  │                                 │<── mesh-configs repo ─────────│
```

### Preshared keys (optional)

PSK adds a symmetric encryption layer on top of WireGuard's Curve25519 key exchange. This provides **post-quantum protection** — even if Curve25519 is broken in the future, recorded traffic cannot be decrypted without the PSK.

**Do you need it?** Probably not. WireGuard is already secure. PSK is useful if you have compliance requirements or want defense-in-depth against future quantum threats. You can always enable it later.

How it works: generate one master secret, distribute it to all fat nodes out of band. Each node independently derives a unique per-link PSK using HKDF-SHA256 — no exchange needed.

```bash
# Generate master (once)
./scripts/gen-keys.sh psk-install /etc/meshctl/psk-master.key
# scp to every fat node (NEVER via the config repo)

# Or use install.sh with --psk to upload during deployment
./scripts/install.sh --psk /path/to/psk-master.key HKG
```

Enable in inventory and agent config:

```yaml
# meshctl.yaml
global:
  psk_enabled: true

# agent.yaml (on each fat node)
psk_master_file: "/etc/meshctl/psk-master.key"
```

RouterOS operators compute per-link PSK manually:

```bash
meshctl psk hk-core hk-edge -m /etc/meshctl/psk-master.key
```

## Requirements

- Go 1.22+ to build
- Fat nodes: Linux, WireGuard, BIRD 2.x / 3.x, chrony/ntpd
- Agent needs `CAP_NET_ADMIN` (WG netlink) + `CAP_NET_RAW` (ICMP + UDP probe)
- RouterOS v7+ for thin nodes

## License

See LICENSE file.
