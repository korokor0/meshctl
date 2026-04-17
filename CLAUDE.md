# CLAUDE.md — meshctl

Automated overlay mesh network controller with dynamic OSPF cost tuning.

## Project overview

**meshctl** is a Go toolset that automates the construction and maintenance of a WireGuard overlay mesh network with BIRD OSPF routing. It replaces manual per-pair WireGuard configuration (O(n²) labor) with a single declarative node inventory, and optionally measures inter-node latency to dynamically adjust OSPF link costs so traffic follows the lowest-latency path.

The system manages heterogeneous nodes: Linux servers (Debian/systemd), MikroTik RouterOS devices, and "static" nodes.

## Design principles

1. **No central node required.** Config generation is a pure function of the inventory YAML and runs anywhere (CI, laptop, any node). Fat nodes independently probe their peers and adjust their own OSPF costs. There is no runtime controller that must stay alive.

2. **GitOps as the primary delivery mechanism.** The inventory YAML and all generated configs live in a dedicated private config repo (separate from the code repo). Fat node agents pull their own config on a schedule or via webhook. RouterOS nodes get `.rsc` scripts from the same config repo.

3. **Config generation over remote control.** Only Linux fat nodes run a live agent. All other nodes receive generated scripts applied manually.

4. **Link-local first, with fallback.** Tunnel interfaces prefer `fe80::` link-local with OSPFv3 AF. Where not supported (RouterOS lacks RFC 5838), fall back to `169.254.x.x` PTP + OSPFv2. All WG interfaces get a `fe80::127:<node_id>/64` address regardless of link mode.

5. **One-way delay, not RTT.** OSPF cost is per-interface outbound. The A→B path may differ from B→A. Each agent measures the forward one-way delay to each peer and uses that for its own cost decision. Agents exchange NTP-synced timestamps via a lightweight UDP probe protocol.

6. **Stability over optimality.** Cost bands with hysteresis prevent OSPF churn from propagating into BGP.

## Architecture overview

```
  ┌─────────────────────┐        ┌──────────────────────┐
  │  meshctl (code repo) │        │ mesh-configs (config) │
  │  Go source, templates│        │ meshctl.yaml          │
  │                     │        │ output/               │
  │  meshctl binary ────┼──run──>│   hk-core/            │
  │  meshctl-agent bin   │        │   hk-edge/            │
  └─────────────────────┘        │   jp-relay/           │
                                 └──────┬───────────────┘
                                        │
                           ┌────────────┼────────────────┐
                           │ git fetch  │ git fetch      │ operator pulls .rsc
                           ▼            ▼                ▼
                     ┌──────────┐ ┌──────────┐    ┌────────────┐
                     │ hk-core  │ │ jp-relay │    │  hk-edge   │
                     │ (fat)    │ │ (fat)    │    │ (RouterOS) │
                     │          │ │          │    │            │
                     │ agent:   │ │ agent:   │    │ operator   │
                     │  pull    │ │  pull    │    │ imports    │
                     │  apply   │ │  apply   │    │ .rsc       │
                     │  probe<──┼──>probe   │    │            │
                     │  adjust  │ │  adjust  │    │ static     │
                     │  cost    │ │  cost    │    │ cost only  │
                     └──────────┘ └──────────┘    └────────────┘
                         UDP probe exchange
                        (peer-to-peer, no center)
```

Two binaries (built from code repo, deployed independently):

| Binary | Runs where | Purpose |
|---|---|---|
| `meshctl` | CI, operator laptop, or any machine | Reads YAML from config repo, computes mesh, generates all config files |
| `meshctl-agent` | Each Linux fat node | Pulls config from config repo, applies WG + BIRD, probes peers, adjusts costs. Subcommand `status` queries the running agent's HTTP endpoint to display per-peer latency and OSPF cost |

## Probe protocol — one-way delay measurement

### Why one-way matters

In an overlay network, the public internet path from A to B may traverse different ASes than B to A. Example: A→B goes through transit provider X (10ms), B→A goes through IX peering Y (50ms). RTT is 60ms, but A's outbound cost to B should reflect 10ms, not 30ms (half RTT). Using half-RTT would over-penalize the A→B direction and under-penalize B→A.

OSPF cost is set per-interface for outbound traffic. Node A sets cost on its `igp-b` interface to reflect the A→B forward delay. Node B independently sets cost on its `igp-a` interface to reflect B→A. These may differ — and they should.

### Protocol

Each agent listens on UDP port 9473 on all WireGuard interfaces. The protocol uses NTP-synced timestamps (all nodes should run NTP/chrony).

```
Probe exchange (similar to NTP timestamp model):

  Node A                          Node B
    │                               │
    │──── Request {seq, T1} ───────>│  T1 = A's send time
    │                               │  T2 = B's recv time
    │                               │  T3 = B's send time
    │<─── Reply {seq, T1, T2, T3} ──│
    │                               │
  T4 = A's recv time

Node A computes:
  forward_delay (A→B) = T2 - T1           ← used for A's OSPF cost on igp-b
  return_delay  (B→A) = T4 - T3           ← informational / cross-check
  rtt                 = (T4-T1) - (T3-T2) ← processing time excluded
  clock_offset_est    = ((T2-T1) - (T4-T3)) / 2

Node B independently probes A and computes its own forward_delay (B→A).
```

### Wire format

```go
// Compact binary format, 32 bytes request / 48 bytes reply.
// All timestamps are nanoseconds since Unix epoch, int64.

type ProbePacket struct {
    Magic   uint32 // 0x4D435450 ("MCTP" - meshctl probe)
    Version uint8  // 1
    Type    uint8  // 0x01 = request, 0x02 = reply
    Seq     uint16 // sequence number (wraps)
    T1      int64  // sender's transmit timestamp (always present)
    T2      int64  // responder's receive timestamp (reply only, 0 in request)
    T3      int64  // responder's transmit timestamp (reply only, 0 in request)
}
```

### Clock offset handling

NTP synchronization is typically accurate to 1-10ms. Since our cost bands have thresholds at 7/25/60/120ms, a few ms of clock error is tolerable. But we add safety:

1. **Sanity check**: If `T2 - T1 < 0` (clock skew makes forward delay appear negative), the measurement is discarded and that probe does not affect the EWMA.

2. **Rolling offset estimation**: Over many probes, maintain a running estimate of clock offset using the NTP formula: `offset = ((T2-T1) + (T3-T4)) / 2`. Apply this correction to improve one-way delay accuracy over time.

3. **Fallback**: If clock offset estimation is unreliable (variance too high, or peer is non-agent), fall back to `rtt / 2`. This is strictly worse but still usable for cost band decisions.

### Probe behavior by peer type

Each thin/static node has a `cost_mode` field that controls how fat nodes determine OSPF cost for that link:

| Peer type | cost_mode | Probe method | OSPF cost behavior |
|---|---|---|---|
| Fat ↔ Fat | (always probe) | Full UDP probe exchange (both directions) | Dynamic: one-way delay → band state machine |
| Fat → Thin/Static | `probe` (default) | ICMP echo (rtt / 2) | Dynamic: rtt/2 → band state machine. `static_cost` used as fallback on failure (instead of penalty 65535) |
| Fat → Thin/Static | `static` | None (skipped) | Fixed: always uses `static_cost`, no probing |

**`cost_mode: probe` (default)** — For fat-to-thin links, the fat node can only measure RTT. It uses `rtt / 2` as the estimated forward delay and feeds it into the cost band state machine. If `static_cost` is also set, it serves as the fallback cost when all ICMP probes fail, avoiding the harsh penalty cost (65535).

**`cost_mode: static`** — The fat node uses the configured `static_cost` unconditionally. No ICMP probes are sent. The agent skips this peer entirely during probe rounds. Useful when the link's latency is well-known and stable, or when ICMP is unreliable.

Example inventory entries:
```yaml
  # Static cost: fat nodes always use cost 150, no probing.
  - name: hk-edge
    type: routeros
    cost_mode: static
    static_cost: 150

  # Probe mode with fallback: ICMP rtt/2, falls back to 200 on failure.
  - name: friend-node
    type: static
    cost_mode: probe        # default, can be omitted
    static_cost: 200        # optional fallback

  # Probe mode without fallback: ICMP rtt/2, penalty 65535 on failure.
  - name: other-node
    type: static
    # cost_mode defaults to "probe", no static_cost → uses penalty on failure
```

Future: a lightweight RouterOS script could implement probe response (read UDP packet, reply with timestamp). RouterOS v7 scripting has `/tool/fetch` and socket support in some versions. This is out of scope for MVP.

### EWMA smoothing for one-way delay

```go
// Per-peer state maintained by each agent.
type PeerProbeState struct {
    // One-way delay (forward direction from this node to peer)
    ForwardDelay      time.Duration // latest EWMA-smoothed forward delay
    RawForwardDelay   time.Duration // last raw measurement

    // RTT (for fallback and diagnostics)
    RTT               time.Duration // latest EWMA-smoothed RTT
    RawRTT            time.Duration // last raw measurement

    // Clock offset
    ClockOffset       time.Duration // rolling estimate of clock difference
    ClockOffsetVar    float64       // variance of clock offset (for reliability check)

    // Probe health
    ConsecutiveFailures int
    LastProbeTime       time.Time
    ProbeMode           ProbeMode   // FullTimestamp or ICMPFallback
}

type ProbeMode int

const (
    ProbeModeFull     ProbeMode = iota // UDP timestamp exchange with agent peer
    ProbeModeICMP                       // ICMP echo fallback (non-agent peer)
)
```

The EWMA update uses the forward delay when available:

```go
func (s *PeerProbeState) Update(forward, rtt time.Duration, alpha float64) {
    if s.ProbeMode == ProbeModeFull && forward > 0 {
        // Use actual one-way delay for cost decisions
        s.RawForwardDelay = forward
        s.ForwardDelay = ewma(s.ForwardDelay, forward, alpha)
    } else {
        // Fallback: estimate forward as half of RTT
        s.RawForwardDelay = rtt / 2
        s.ForwardDelay = ewma(s.ForwardDelay, rtt/2, alpha)
    }
    s.RawRTT = rtt
    s.RTT = ewma(s.RTT, rtt, alpha)
}
```

## Cost band design

### Why not continuous cost

`cost = base + delay * multiplier` causes OSPF to reconverge every probe cycle due to natural jitter. Each reconvergence can trigger BGP UPDATE.

### Quantized bands with hysteresis

Cost is based on the **forward one-way delay** (not RTT):

```go
type CostBand struct {
    UpThreshold   time.Duration // forward delay must exceed this to enter (from below)
    DownThreshold time.Duration // forward delay must drop below this to leave (going down)
    Cost          uint32
    HoldCount     int           // consecutive probes in new band before switching
}

var DefaultBands = []CostBand{
    {UpThreshold: 0,                    DownThreshold: 0,                    Cost: 20,   HoldCount: 5},
    {UpThreshold: 4*time.Millisecond,   DownThreshold: 2*time.Millisecond,   Cost: 80,   HoldCount: 5},
    {UpThreshold: 12*time.Millisecond,  DownThreshold: 8*time.Millisecond,   Cost: 160,  HoldCount: 5},
    {UpThreshold: 30*time.Millisecond,  DownThreshold: 20*time.Millisecond,  Cost: 250,  HoldCount: 5},
    {UpThreshold: 60*time.Millisecond,  DownThreshold: 40*time.Millisecond,  Cost: 350,  HoldCount: 5},
    {UpThreshold: 100*time.Millisecond, DownThreshold: 70*time.Millisecond,  Cost: 480,  HoldCount: 5},
    {UpThreshold: 160*time.Millisecond, DownThreshold: 120*time.Millisecond, Cost: 640,  HoldCount: 5},
    {UpThreshold: 220*time.Millisecond, DownThreshold: 180*time.Millisecond, Cost: 840,  HoldCount: 5},
    {UpThreshold: 300*time.Millisecond, DownThreshold: 260*time.Millisecond, Cost: 1100, HoldCount: 5},
}
```

Note: thresholds are one-way delay (roughly half of RTT). Cost increments are larger at low latency (where quality differences matter most) and increase at high latency (convex shape) to allow OSPF to prefer multi-hop PoP relay paths when direct links degrade.

Unreachable peers (N consecutive failures, default 3) get penalty cost 65535, unless the peer has a `static_cost` configured — in which case the static cost is used as fallback instead of the penalty. Peers with `cost_mode: static` bypass the band state machine entirely and always use their `static_cost`.

### Asymmetric cost example

```
Scenario: A→B via cheap transit (5ms), B→A via expensive IX (25ms)

Node A measures:
  forward A→B = 5ms  → band 1, cost 50
  (return B→A = 25ms, informational only)

Node B measures:
  forward B→A = 25ms → band 3, cost 200
  (return A→B = 5ms, informational only)

OSPF state:
  A's igp-b interface: cost 50  (good outbound path)
  B's igp-a interface: cost 200 (poor outbound path)

Traffic A→B prefers the direct link (low cost from A's perspective).
Traffic B→A may route through a third node C if B→C→A has lower total cost.
This is CORRECT behavior — asymmetric costs reflect asymmetric reality.
```

### Bandwidth-aware cost adjustment

In addition to latency-based cost bands, OSPF cost can include a bandwidth penalty for low-bandwidth links. This uses the standard OSPF `auto-cost` formula (Cisco/Juniper: `reference_bandwidth / interface_bandwidth`) but only penalizes links below a configurable threshold.

```yaml
global:
  bandwidth_threshold: 300    # Mbps — links at or above this get no penalty (default: 300)
  reference_bandwidth: 3000   # Mbps — reference for cost formula (default: 3000)
```

Per-node bandwidth declaration:

```yaml
nodes:
  - name: hk-core
    bandwidth: 10000          # 10Gbps — no penalty (above threshold)
  - name: hk-edge
    bandwidth: 100            # 100Mbps — penalized
  # Nodes without bandwidth: treated as bandwidth_threshold (no penalty)
```

**Formula:**

```
link_bw = min(A.bandwidth, B.bandwidth)

if link_bw >= bandwidth_threshold:
    bandwidth_penalty = 0
else:
    bandwidth_penalty = reference_bw / link_bw - reference_bw / bandwidth_threshold

final_ospf_cost = latency_band_cost * (1000 + bandwidth_penalty * 10) / 1000
```

The penalty is applied **multiplicatively**: each unit of penalty ≈ +1% cost. This preserves the percentage impact across all cost bands — a 50Mbps link pays the same proportional penalty whether the base latency cost is 20 or 640.

**Examples** (reference=3000, threshold=300):

| link_bw | penalty | % increase | band cost 80 → final | band cost 640 → final |
|---|---|---|---|---|
| 1000 Mbps | 0 | 0% | 80 | 640 |
| 300 Mbps | 0 | 0% | 80 | 640 |
| 200 Mbps | 5 | +5% | 84 | 672 |
| 100 Mbps | 20 | +20% | 96 | 768 |
| 50 Mbps | 50 | +50% | 120 | 960 |

The bandwidth penalty is computed at config generation time (static) and emitted into wireguard.json. The agent applies it multiplicatively to the latency band cost. Nodes without an explicit `bandwidth` field default to `bandwidth_threshold`, ensuring backward compatibility (zero penalty).

### BGP stability measures

1. **BGP sessions on loopback.** Peer via stable loopback addresses.
2. **Fixed MED on export.** `bgp_med = 0` prevents IGP cost changes from causing eBGP UPDATE.
3. **next-hop self + stable local-pref for iBGP.** IGP changes affect forwarding only, not route selection.
4. **Route flap dampening (optional).**

## Architecture layers

### Layer A — Mesh config generation

`meshctl generate` is a pure function: YAML in, config files out. No state, no network access.

Output artifacts per node type:

| Node type | Artifacts | Delivery |
|---|---|---|
| Linux (fat) | `bird-meshctl.conf`, `wireguard.json` | Agent pulls from git |
| RouterOS (thin) | `.rsc` scripts | Operator pulls from git, imports |
| Static | Reference snippets + README | Operator configures manually |

### Layer B — Latency probing and dynamic OSPF cost

Fully decentralized. Each fat node agent independently:
1. Sends/receives UDP probes to/from all fat peers (one-way delay)
2. Sends ICMP echo to non-agent peers (RTT fallback)
3. Feeds forward delay into cost band state machine
4. Rewrites BIRD include when a band changes

No coordination needed. Each node makes its own cost decisions from its own measurements.

### Layer C — Topology optimization (future)

Not in MVP. Would use exported probe data to prune links.

## Addressing scheme

### Tunnel interface addressing

Each node has a `node_id` (integer, explicit or auto-assigned alphabetically). Addresses are deterministically derived:

- **V4LL**: `linklocal_v4_range` base + `node_id`. E.g. base `169.254.0.0` + node_id `2` = `169.254.0.2`. PTP format on wire: `ip addr replace 169.254.0.4 peer 169.254.0.2/32 dev igp-hkg`
- **Fe80**: `fe80::127:<node_id>` assigned to ALL WG interfaces (both fe80 and V4LL mode). Uses `ip -6 addr replace` for idempotency.

**Mode 1: IPv6 link-local only (preferred)** — Linux-to-Linux links only. `fe80::127:<node_id>/64` + OSPFv3 AF (RFC 5838).

**Mode 2: 169.254.x.x PTP + OSPFv2 (fallback)** — Any link involving RouterOS or static node. Both V4LL PTP and fe80 addresses are assigned.

Selection: both endpoints Linux → mode 1; otherwise → mode 2. A Linux node can have both modes simultaneously (BIRD runs OSPFv3 AF + OSPFv2 instances, same kernel table).

### Loopback addressing

One routable loopback per node from configured range (e.g. `10.200.255.0/24`). Used for router-id, BGP endpoint, management. Announced as /32 stub.

## Node types

### Fat node (`type: linux`)

Runs `meshctl-agent`. Capabilities:
- Git fetch + reset for config sync (handles force pushes)
- WireGuard netlink management (wgctrl-go)
- BIRD control socket for OSPF cost updates
- UDP probe server/client on port 9473 (one-way delay measurement)
- ICMP echo for non-agent peers with `cost_mode: probe` (RTT fallback)
- Independent cost band adjustment
- Honors `cost_mode: static` peers by skipping probes and using fixed cost

### Thin node (`type: routeros`)

RouterOS v7+. Receives generated `.rsc` scripts. Cannot participate in UDP probe protocol (no agent). Fat peers determine cost based on this node's `cost_mode`:
- `cost_mode: probe` (default): fat peers measure RTT via ICMP, use rtt/2 estimate. Optional `static_cost` as fallback on probe failure.
- `cost_mode: static`: fat peers use `static_cost` unconditionally, no probing.

### Static node (`type: static`)

Cannot be auto-configured. Gets reference snippets. Same `cost_mode` options as thin nodes:
- `cost_mode: probe` (default): fat peers use ICMP rtt/2, optional `static_cost` fallback.
- `cost_mode: static`: fat peers use `static_cost`, no probing.

### Nodes without public IP (NAT traversal)

Nodes behind NAT with no public IP are supported. The `endpoint` section is optional — omit it entirely for NAT nodes. Constraints:

- **At least one side of each link must have a public endpoint.** `meshctl generate` will error if both sides lack endpoints.
- **NAT nodes initiate connections.** The WireGuard peer entry on the public side has no `endpoint` — it waits for the NAT node to connect.
- **Use `peers_with`** to explicitly list which public nodes the NAT node connects to.
- **`persistent_keepalive`** (from `global.wg_persistent_keepalive`) keeps the tunnel alive through NAT.
- **Underlay routes** are not generated for peers without endpoint IPs (no destination to route to).

## Node inventory format

See `examples/meshctl.example.yaml` for a complete annotated example.

## Key interfaces and types

```go
type NodeType string

const (
    NodeTypeLinux    NodeType = "linux"
    NodeTypeRouterOS NodeType = "routeros"
    NodeTypeStatic   NodeType = "static"
)

type CostMode string

const (
    CostModeProbe  CostMode = "probe"  // ICMP rtt/2 for thin peers, UDP for fat peers (default)
    CostModeStatic CostMode = "static" // fixed cost, no probing
)

type LinkMode int

const (
    LinkModeFe80 LinkMode = iota // IPv6 link-local + OSPFv3 AF
    LinkModeV4LL                 // 169.254.x.x PTP + OSPFv2
)

type Link struct {
    NodeA string       // sorted: A < B
    NodeB string
    Mode  LinkMode
    AddrA netip.Prefix // only for LinkModeV4LL (/32 PTP)
    AddrB netip.Prefix // only for LinkModeV4LL (/32 PTP)
}

// ConfigGenerator produces platform-specific config files.
type ConfigGenerator interface {
    GenerateWireguard(node *Node, peers []WGPeerConfig) ([]byte, error)
    GenerateOSPF(node *Node, links []Link) ([]byte, error)
    GenerateAddressing(node *Node, links []Link) ([]byte, error)
    GenerateFull(node *Node, peers []WGPeerConfig, links []Link) ([]byte, error)
}
// Implementations: BIRDGenerator, RouterOSGenerator, StaticSnippetGenerator

// CostEngine runs locally in each agent.
type CostEngine struct {
    bands   []CostBand
    penalty uint32
    alpha   float64
    state   map[string]*LinkCostState
}

type LinkCostState struct {
    ForwardDelay    time.Duration // EWMA-smoothed one-way forward delay
    RTT             time.Duration // EWMA-smoothed RTT (for fallback)
    CurrentBand     int
    PendingBand     int
    HoldCounter     int
    Failures        int
    StaticCost      *uint32       // if set, always use this cost (cost_mode: static)
    FallbackCost    *uint32       // if set, use instead of penalty on failure (cost_mode: probe + static_cost)
    BandwidthPenalty uint32       // multiplicative penalty from low bandwidth (0 if link_bw >= threshold)
}

// ProbeServer listens on UDP port and responds to probe requests.
type ProbeServer struct {
    listenPort int
    conn       *net.UDPConn
}

// ProbeClient sends probes to peers and collects results.
type ProbeClient struct {
    peers     map[string]*PeerProbeState
    probePort int
    timeout   time.Duration
}
```

## Repositories

Two separate git repos with different lifecycles:

| Repo | Contents | Change frequency | Access |
|---|---|---|---|
| **meshctl** (code) | Go source, templates, build scripts | Low (versioned releases) | Can be public / open-source |
| **mesh-configs** (config) | meshctl.yaml, output/, agent.yaml per node | High (every topology change) | Private (contains pubkeys, endpoints) |

### Code repo structure

```
meshctl/                       # github.com/yourorg/meshctl
├── CLAUDE.md
├── go.mod
├── go.sum
├── cmd/
│   ├── meshctl/               # config generator binary
│   │   └── main.go            # subcommands: generate, validate, diff, show-mesh, psk
│   └── meshctl-agent/         # node agent binary
│       └── main.go            # subcommands: (default=run), status
├── internal/
│   ├── config/                # YAML inventory parsing and validation
│   │   ├── config.go
│   │   └── config_test.go
│   ├── mesh/                  # mesh computation
│   │   ├── links.go           # peer pair enumeration, link mode selection
│   │   ├── addressing.go      # node_id-based deterministic PTP addressing
│   │   └── mesh_test.go
│   ├── generate/              # config generators
│   │   ├── generator.go       # interface
│   │   ├── bird.go            # BIRD config (OSPFv3 AF + OSPFv2)
│   │   ├── routeros.go        # RouterOS .rsc scripts
│   │   ├── static.go          # reference snippets
│   │   ├── templates/
│   │   │   ├── bird_ospfv3.tmpl
│   │   │   ├── bird_ospfv2.tmpl
│   │   │   ├── ros_wireguard.rsc.tmpl
│   │   │   ├── ros_ospf.rsc.tmpl
│   │   │   └── ros_full.rsc.tmpl
│   │   └── generate_test.go
│   ├── agent/                 # agent main loop
│   │   ├── agent.go           # config sync + probe + cost adjust
│   │   ├── fetch.go           # multi-source config fetch (git, http, local)
│   │   ├── apply.go           # WG netlink + BIRD apply
│   │   └── agent_test.go
│   ├── probe/                 # latency probing
│   │   ├── protocol.go        # probe packet format, marshal/unmarshal
│   │   ├── server.go          # UDP listener, responds to incoming probes
│   │   ├── client.go          # sends probes, collects T4, computes delays
│   │   ├── icmp.go            # ICMP echo fallback for non-agent peers
│   │   └── probe_test.go
│   ├── cost/                  # cost band engine
│   │   ├── engine.go          # band state machine (uses forward delay)
│   │   └── engine_test.go
│   └── bird/                  # BIRD control socket client
│       ├── client.go
│       └── client_test.go
├── examples/
│   ├── meshctl.example.yaml   # example inventory (sanitized, no real keys)
│   └── agent.example.yaml     # example agent config
├── scripts/
│   ├── install.sh             # one-command remote deploy: install.sh <ssh-target>
│   ├── install-agent.sh       # legacy deploy script (use install.sh instead)
│   ├── bootstrap-node.sh      # local node init: create dirs + generate WG key
│   ├── gen-keys.sh            # generate WireGuard keypairs and PSK master
│   └── init-config-repo.sh    # scaffold a new mesh-configs repo
├── deployments/
│   └── meshctl-agent.service  # systemd unit template
├── .goreleaser.yaml           # cross-compile releases
└── Makefile
```

### Config repo structure

```
mesh-configs/                  # git.internal/yourorg/mesh-configs (PRIVATE)
├── meshctl.yaml               # node inventory (source of truth)
├── output/                    # generated by `meshctl generate`, committed
│   ├── hk-core/              # fat node
│   │   ├── bird-meshctl.conf
│   │   └── wireguard.json
│   ├── hk-edge/              # thin node (RouterOS)
│   │   ├── wireguard.rsc
│   │   ├── ospf.rsc
│   │   ├── addresses.rsc
│   │   ├── full-setup.rsc
│   │   └── README.txt
│   ├── jp-relay/             # fat node
│   │   ├── bird-meshctl.conf
│   │   └── wireguard.json
│   └── friend-node/          # static node
│       ├── wireguard.conf.snippet
│       └── README.txt
├── agents/                    # per-node agent config (optional, can also be local-only)
│   ├── hk-core.yaml
│   └── jp-relay.yaml
└── .gitlab-ci.yml             # CI: run meshctl generate on meshctl.yaml change
    # (or .github/workflows/generate.yml)
```

CI pipeline for the config repo:

```yaml
# .gitlab-ci.yml in mesh-configs repo
generate:
  image: registry.example.com/meshctl:latest  # or download binary from release
  stage: build
  script:
    - meshctl generate --config meshctl.yaml
    - git add output/
    - git diff --cached --quiet || git commit -m "meshctl: regenerate configs"
    - git push
  only:
    changes:
      - meshctl.yaml
```

## Agent operation

```
meshctl-agent --config /etc/meshctl/agent.yaml

Three independent loops:

1. CONFIG SYNC (every config_interval, default 5m, or on SIGHUP / systemctl reload):
   a. Try repo sources in order (git → git mirror → http → local)
      Git sources use `fetch --depth 1` + `reset --hard` (handles force pushes)
   b. On success: update local cache, read output/<my-node-name>/
   c. On all-fail: log warning, use cached config, skip apply
   d. Diff WG peers, apply adds/removes/updates via netlink
   e. Write BIRD include if changed, birdc configure

2. PROBE (every probe_interval, default 30s):
   For each peer:
     If peer has cost_mode=static → skip (cost is fixed)
     If peer is fat (has agent)   → UDP timestamp probe (one-way delay)
     If peer is thin/static       → ICMP echo (RTT fallback, use rtt/2)
   Also: respond to incoming UDP probes from other fat peers

3. COST ADJUSTMENT (after each probe round):
   For each peer:
     If peer has cost_mode=static → no action (always uses static_cost)
     If probe succeeded → feed forward delay (or rtt/2) into band state machine
     If probe failed + static_cost set → use static_cost as fallback (not penalty)
     If probe failed + no static_cost  → use penalty cost (65535)
     If any cost changed → rewrite BIRD include → birdc configure
```

See `examples/agent.example.yaml` for agent configuration. Sources are tried in order (git → git mirror → http → local); first success wins.

### Config sync failure behavior

Git failure does NOT affect running tunnels or routing. The three agent loops (config sync, probe, cost adjust) run as independent goroutines. A git fetch that hangs or fails only blocks the config sync loop; probing and cost adjustment continue uninterrupted.

```
All sources reachable  → fetch from first, update cache, apply if changed
Primary down           → try next source, update cache, apply if changed
All sources down       → log warning, keep running with cached config
Cache empty + all down → FATAL on first boot only (no config to run)
                         On subsequent boots, systemd restart will retry
```

Repeated all-source failures trigger exponential backoff (30s → 10min cap) to avoid hammering git servers. The backoff counter resets on any successful fetch.

The agent exposes health and per-peer status via file and optional HTTP endpoint:
- `GET /health` — agent health (uptime, config age, NTP status, peer count)
- `GET /peers` — per-peer latency, OSPF cost, cost band, probe mode/status

Query with `meshctl-agent status [--addr :9474]` or `--json` for scripted use.

## BIRD integration

Fat node include structure:

```
# /etc/bird/bird.conf (operator-managed)
router id 10.200.255.1;
protocol device {}
protocol direct { ipv4; ipv6; }
protocol kernel k4 { ipv4 { export all; }; }
protocol kernel k6 { ipv6 { export all; }; }
protocol static lo4 { ipv4; route 10.200.255.1/32 via "lo"; }

# Pipe IGP tables to master (meshctl OSPF routes live in igptable4/6)
protocol pipe igp4 { table igptable4; peer table master4; import none; export all; }
protocol pipe igp6 { table igptable6; peer table master6; import none; export all; }

include "/etc/bird/meshctl.conf";           # agent-managed (OSPF + table declarations)
include "/etc/bird/meshctl-underlay.conf";  # agent-managed (underlay static routes)
include "/etc/bird/bgp.conf";               # operator-managed
```

`meshctl.conf` declares `igptable4`/`igptable6` and contains OSPFv3 AF (fe80 links) + OSPFv2 (169.254 links) importing/exporting to those tables. The operator pipes them to master4/master6 in `bird.conf`. Table names are configurable via `global.igp_table4`/`igp_table6` (defaults: `igptable4`, `igptable6`). Agent rewrites on config sync or cost band change. `birdc configure` is graceful.

`meshctl-underlay.conf` contains `protocol static meshctl_underlay4/6` blocks for underlay routes with `krt_prefsrc`. These target master4/master6 directly since they need to be installed in the kernel FIB via `protocol kernel`.

## Underlay static routes

Fat nodes that set `underlay` in the inventory get auto-generated BIRD static routes for reaching peer endpoints on the physical network. These routes pin the source address via `krt_prefsrc` so the kernel uses the correct interface/IP for WireGuard tunnel traffic.

### Why

On multi-homed hosts the kernel's default source address selection may pick the wrong IP when sending WireGuard UDP packets to a peer, causing return traffic to arrive on a different interface or be dropped by RPF. Explicit `krt_prefsrc` ensures the correct source is always used.

### Config

```yaml
nodes:
  # Simplest: prefsrc defaults to endpoint IPs
  - name: hk-core
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    underlay: {}    # prefsrc4=1.2.3.4, prefsrc6=2001:db8::1 (from endpoint IPs)

  # Override prefsrc when source differs from endpoint
  - name: jp-relay
    type: linux
    endpoint:
      ipv4: "198.51.100.5"
    underlay:
      prefsrc4: "ens3"           # interface name → agent picks primary IP
```

Each `prefsrc` field accepts:
- **Empty (default)** — uses the node's own endpoint IP for that address family
- **Literal IP** — used as-is for `krt_prefsrc`
- **Interface name** (e.g. `"ens3"`) — agent runs `ip addr show dev ens3` and picks the first non-deprecated address, following the Linux kernel's source address selection rules (primary address first, skip addresses with `preferred_lft 0sec`)
- **`"auto"`** — agent first checks `ip route show default` for a `src` field; if the default route has no explicit src, extracts the `dev` and picks the primary IP from that interface; as a last resort falls back to `ip route get` to let the kernel select the source

When multiple addresses exist on an interface, address selection follows the Linux kernel's behavior: addresses are ordered by the kernel with the primary (first-added) address listed first; deprecated addresses (`preferred_lft 0sec`) are skipped; the first remaining address is used.

### Flow

1. `meshctl generate` reads `underlay.prefsrc4`/`prefsrc6` and peer endpoint IPs, emits `underlay_routes` in `wireguard.json` (prefsrc is passed through as-is — resolution happens at runtime).
2. Agent reads `underlay_routes`, resolves each prefsrc value (IP/interface/auto) to a concrete IP.
3. Agent runs `ip route get <dest>` to detect the current default gateway for each peer endpoint.
4. Agent writes `/etc/bird/meshctl-underlay.conf` with `protocol static meshctl_underlay4/6` blocks.
5. Routes use `import all; export none;` — they are installed in the kernel FIB but not redistributed into OSPF.

### Dual-stack endpoint selection

Nodes declare IPv4 and/or IPv6 addresses directly in the `endpoint` struct. The generator picks the best endpoint per peer pair:

- Both sides have IPv6 → use IPv6 endpoint
- Only one protocol in common → use that
- Domain set → use domain as-is for WireGuard

The port comes from `wg_listen_port` or `global.wg_listen_port`. Per-peer listen ports are auto-assigned from each node's base port in alphabetical peer order. Use `wg_peer_port` on a node to fix the port that all peers use to listen for connections from that node, preventing port shifts when new nodes join the mesh.

```yaml
  # Dual-stack: v4-only peers get v4 endpoint, v6-capable peers get v6
  - name: hk-core
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"

  # Domain endpoint: domain for WG, IPs for underlay + endpoint selection
  - name: us-west
    endpoint:
      domain: "us-west.example.com"
      ipv4: "203.0.113.50"
      ipv6: "2001:db8:cafe::1"
      ddns: false     # reserved: true enables periodic re-resolution by agent
    underlay:
      prefsrc4: "auto"
      prefsrc6: "auto"
```

Domain endpoints are passed as-is to WireGuard (WireGuard resolves them). The `ipv4`/`ipv6` fields in the `endpoint` struct are used for endpoint selection and underlay route generation. If no IP is provided, no underlay route is generated for that peer.

## RouterOS .rsc design

Idempotent scripts with check-before-add. OSPF interface-templates remove+recreate for cost updates. See full example in previous sections.

No gRPC. No protobuf. No CGO. Cross-compiles for linux/amd64, linux/arm64. Dependencies in `go.mod`. Build, deploy and day-to-day workflow instructions are in `README.md`.

## Coding conventions

- All code comments in English
- Error wrapping: `fmt.Errorf("operation: %w", err)`
- Context propagation: `context.Context` as first argument
- Logging: `log/slog` with structured fields
- Testing: table-driven, mock via interface
- No global state; dependency injection via struct fields
- Config read-only after load; runtime state in separate structs

## Key material and PSK derivation

WireGuard private keys and the PSK master secret are **never** stored in
`meshctl.yaml` or the config repo. They are node-local (private key) or
mesh-wide out-of-band (PSK master) secrets.

### Private keys

Each fat node holds its own private key at `/etc/meshctl/wireguard.key`
(mode 0600), generated once per node via `scripts/gen-keys.sh wg-install`.
Only the public key is copied into `meshctl.yaml`. On interface creation
the agent calls `wg set <iface> private-key /etc/meshctl/wireguard.key`.

### Preshared keys via HKDF

Enabling `global.psk_enabled: true` in the inventory causes the generator
to emit `"psk_required": true` in each fat node's `wireguard.json`. Every
fat node must then have `psk_master_file` configured (default
`/etc/meshctl/psk-master.key`). The same master file must be distributed
to every fat node — out of band, never via the config repo.

For each peer, both endpoints independently derive an identical 32-byte
PSK using HKDF-SHA256:

```
PRK = HMAC-SHA256(salt=zero, IKM=master)
PSK = HMAC-SHA256(PRK, "meshctl-psk-v1|<nodeA>|<nodeB>" || 0x01)[:32]
```

The two node names are sorted lexicographically before concatenation, so
`Derive(m, A, B) == Derive(m, B, A)`. No coordination or exchange is
needed — both sides compute the same key locally from the shared master.

RouterOS nodes cannot run the agent, so their operators compute the
per-link PSK manually using `meshctl psk <node-a> <node-b> -m
/path/to/psk-master.key` and paste it into the generated `.rsc`.

## Keys and credentials

meshctl uses several distinct keys. Understanding which is which prevents confusion during deployment.

| Key | What it is | Where it lives | Purpose |
|---|---|---|---|
| **WireGuard private key** | Curve25519 private key | `/etc/meshctl/wireguard.key` on each node (0600) | Encrypts WireGuard tunnels |
| **WireGuard public key** | Derived from private key | `meshctl.yaml` `pubkey` field | Peers use it to identify this node |
| **Deploy key** | Ed25519 SSH key (read-only) | `/etc/meshctl/deploy_key` on each node (0600) | Agent uses it to `git fetch` config repo |
| **PSK master** (optional) | 32-byte symmetric secret | `/etc/meshctl/psk-master.key` on each node (0600) | Derives per-link PSKs for post-quantum protection |
| **Operator SSH key** | Operator's personal SSH key | `~/.ssh/` on operator laptop | Operator uses it to SSH into nodes for setup |

### Deploy key

The deploy key is a dedicated read-only SSH key that lets the agent pull from the private config repo. It is NOT the operator's personal SSH key.

```
Operator laptop                   Remote node                     GitHub/GitLab
  │                                 │                               │
  │── ssh (operator key) ──────────>│                               │
  │   scp deploy_key ─────────────>│                               │
  │                                 │── git fetch (deploy_key) ────>│
  │                                 │<── mesh-configs repo ─────────│
```

Setup: generate once with `ssh-keygen -t ed25519`, add the public key to the config repo as a read-only deploy key (GitHub → Settings → Deploy keys). All fat nodes share the same deploy key. The `install.sh` script uploads the private key to `/etc/meshctl/deploy_key` automatically.

### PSK (Pre-Shared Key)

PSK adds a symmetric encryption layer on top of WireGuard's Curve25519 key exchange, providing post-quantum protection. If Curve25519 is broken by future quantum computers, recorded traffic still cannot be decrypted without the PSK.

PSK is optional — WireGuard is already secure without it. Enable if compliance requires it or for defense-in-depth. Can be added at any time without disrupting existing tunnels.

## Security considerations

- Agent git SSH key (deploy key) should be read-only, with access only to the config repo
- Agent needs `CAP_NET_ADMIN` (WG netlink) + `CAP_NET_RAW` (ICMP + UDP probe)
- UDP probe port 9473 is only on WireGuard interfaces (encrypted tunnel), not exposed on public interfaces
- Generated `.rsc` must NOT contain WG private keys
- Private keys live only on the node (`/etc/meshctl/wireguard.key`, mode 0600) and are never in the config repo
- PSK master is shared across fat nodes out of band. Per-link PSK is derived via HKDF on each node from the master, so the master itself never traverses the mesh
- Deploy key is read-only and scoped to the config repo only — compromise of a deploy key does not grant write access

## Open design questions

1. **Probe data export for Layer C.** Agents could write latency matrices to repo `probes/` dir. Not needed for MVP.
2. **Dual-stack route redistribution.** BIRD with OSPFv3 AF + OSPFv2 exporting to same channel. Needs integration testing.
3. **RouterOS two-phase key setup.** First import creates WG interfaces (auto keypair). Operator copies pubkey to inventory. Second generate + import adds peers.
4. **NTP requirement.** chrony/ntpd should be documented as a hard prerequisite for fat nodes. Consider adding a startup check in the agent that warns if clock sync is poor (offset > 10ms).
5. **Probe over WireGuard.** The UDP probe runs through the WG tunnel, so it measures tunnel + underlay delay combined. This is what we want (OSPF cost should reflect actual forwarding delay). But if WG itself adds jitter (CPU-bound encryption on low-power devices), that will show up in measurements — which is arguably correct since it affects real traffic too.
