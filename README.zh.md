# meshctl

自动化 Overlay Mesh 网络控制器，支持基于延迟的动态 OSPF 开销调优。

meshctl 将手动逐对配置 WireGuard 的 O(n²) 工作量简化为一份声明式 YAML 清单。它能为 Linux、MikroTik RouterOS 和静态节点生成对应平台的配置文件，并可选地在每个节点上运行 Agent，通过测量节点间延迟来动态调整 OSPF 链路开销。

## 工作原理

```
  YAML 清单 ──→ meshctl generate ──→ 各节点配置文件
                                          │
                      ┌───────────────────┼──────────────────┐
                      ▼                   ▼                  ▼
                 Linux (胖节点)      Linux (胖节点)       RouterOS
                 meshctl-agent       meshctl-agent       运维人员导入 .rsc
                   │                   │
                   └── UDP 探测 ───────┘
                   (单向延迟测量)
```

两个二进制文件，两个仓库：

| 二进制文件 | 用途 |
|---|---|
| `meshctl` | 配置生成器 — 读取 YAML，输出 WireGuard + BIRD + RouterOS 配置 |
| `meshctl-agent` | 节点 Agent — 拉取配置、应用 WG/BIRD、探测对端、调整 OSPF 开销 |

**代码仓库**（本仓库）包含 Go 源码。另有一个独立的**配置仓库**（私有），存放 `meshctl.yaml` 和生成的输出文件。

## 快速开始

### 构建

```bash
make build
# 输出: bin/meshctl, bin/meshctl-agent
```

### 生成配置

```bash
# 复制并编辑示例清单
cp examples/meshctl.example.yaml meshctl.yaml
vim meshctl.yaml

# 验证配置
bin/meshctl validate --config meshctl.yaml

# 预览拓扑
bin/meshctl show-mesh --config meshctl.yaml

# 生成所有配置
bin/meshctl generate --config meshctl.yaml

# 查看变更
bin/meshctl diff --config meshctl.yaml
```

### 部署 Agent 到 Linux 节点

```bash
# 一条命令搞定（HKG 是 ~/.ssh/config 里的别名，也可以用 root@1.1.1.1）
./scripts/install.sh HKG

# 指定选项
./scripts/install.sh --node hk-core --repo-url git@github.com:org/mesh-configs.git \
    --deploy-key ~/.ssh/meshctl_deploy_key HKG

# 或者设好环境变量后只传 SSH 目标
export MESHCTL_REPO_URL="git@github.com:yourorg/mesh-configs.git"
export MESHCTL_DEPLOY_KEY="~/.ssh/meshctl_deploy_key"
./scripts/install.sh HKG
```

脚本自动检测远端架构、上传正确的 binary、在节点上生成 WireGuard 密钥对，并打印公钥供你填入 `meshctl.yaml`。

执行前先 `make release` 交叉编译 agent binary。

### 更新 agent 可执行文件

```bash
# 替换 binary + 重启服务
./scripts/install.sh upgrade HKG

# 只替换 binary，之后手动重启
./scripts/install.sh upgrade --no-restart HKG

# 批量并行升级多台节点（最多 6 个并发）
./scripts/install.sh upgrade HKG TYO LAX

# 滚动升级——逐台执行，遇到失败立即中止
./scripts/install.sh upgrade --rolling HKG TYO LAX
```

### 应用到 RouterOS

```bash
scp output/hk-edge/full-setup.rsc admin@192.168.88.1:/
ssh admin@192.168.88.1 "/import full-setup.rsc"
```

## 节点类型

| 类型 | 配置下发方式 | 探测方式 | OSPF 开销 |
|---|---|---|---|
| `linux`（胖节点） | Agent 从 git 拉取 | UDP 单向延迟 | 动态调整（分级状态机） |
| `routeros`（瘦节点） | 运维人员导入 .rsc | 可配置（见下方） | 可配置 |
| `static` | 参考配置片段 | 可配置（见下方） | 可配置 |

### 瘦节点/静态节点的开销模式

胖节点支持两种方式确定对瘦/静态节点的 OSPF 开销，通过节点的 `cost_mode` 字段控制：

**`cost_mode: probe`**（默认） — 基于 ICMP rtt/2 动态调整：
```yaml
  - name: friend-node
    type: static
    cost_mode: probe          # 默认值，可省略
    static_cost: 200          # 可选：ICMP 探测全部失败时的回退开销（替代惩罚值 65535）
```
胖节点发送 ICMP echo，用 `rtt/2` 作为估算的前向延迟，输入开销分级状态机。若设置了 `static_cost` 且所有探测失败，使用该值作为回退开销，而非惩罚值（65535）。

**`cost_mode: static`** — 固定开销，不探测：
```yaml
  - name: hk-edge
    type: routeros
    cost_mode: static
    static_cost: 150          # cost_mode 为 static 时必填
```
胖节点无条件使用开销 150。不发送 ICMP，不进行探测，不参与分级切换。Agent 在探测轮次中完全跳过此节点。

## 清单格式

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
  # igp_table4: igptable4    # BIRD 的 OSPF IPv4 路由表名
  # igp_table6: igptable6    # BIRD 的 OSPF IPv6 路由表名
  # wg_iface_prefix: igp-    # WireGuard 接口名前缀（默认 "igp-"）
  # bandwidth_threshold: 300 # Mbps — 低于此值的链路会额外加权（默认 300）
  # reference_bandwidth: 3000 # Mbps — Cisco 风格 auto-cost 公式参考值（默认 3000）

nodes:
  # node_id: 确定性地址分配。V4LL = base + node_id, fe80 = fe80::127:<node_id>。
  # 省略时按字母序自动分配。显式设置可防止增删节点时地址漂移。

  # 双栈节点：生成器根据对端能力选择 v6 或 v4 端点
  - name: hk-core
    type: linux
    node_id: 1                # → 169.254.0.1, fe80::127:1
    bandwidth: 10000          # Mbps。省略则默认等于 bandwidth_threshold（无惩罚）
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    loopback: 10.200.255.1
    announce:
      - 192.168.1.0/24
    pubkey: "aB3d...="
    underlay: {}              # prefsrc 默认用 endpoint IP

  - name: hk-edge
    type: routeros
    node_id: 2                # → 169.254.0.2, fe80::127:2
    bandwidth: 100            # 100Mbps — 低于阈值，加权
    endpoint:
      ipv6: "2001:db8::2"
    loopback: 10.200.255.2
    pubkey: "cD5f...="
    wg_listen_port: 13231
    wg_peer_port: 60001       # 固定端口：所有 peer 的 igp-hk-edge 接口监听 60001
    cost_mode: static         # 固定开销，不探测
    static_cost: 150

  - name: friend-node
    type: static
    node_id: 10               # → 169.254.0.10, fe80::127:a
    endpoint:
      ipv4: "203.0.113.99"
    loopback: 10.200.255.10
    pubkey: "eF7g...="
    cost_mode: probe          # ICMP rtt/2，附带静态回退
    static_cost: 200

link_policy:
  mode: full
```

完整示例见 `examples/meshctl.example.yaml`。

### NAT 后节点（无公网 IP）

没有公网 IP 的节点可以加入 mesh 网络。省略 `endpoint` 部分即可：

```yaml
  - name: home-node
    type: linux
    loopback: 10.200.255.20
    pubkey: "KEY="
    peers_with:
      - hk-core
      - jp-relay
```

NAT 节点主动向对端发起 WireGuard 连接。每条链路至少一端必须有公网端点——否则 `meshctl generate` 会报错。使用 `peers_with` 指定连接哪些公网节点。`persistent_keepalive` 保持隧道穿越 NAT。

## 双栈端点

节点可以同时有 IPv4 和 IPv6 地址，直接在 `endpoint` 结构体中声明：

```yaml
  - name: hk-core
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
```

生成器为每个 peer 选择最佳端点——双方都支持 IPv6 时优先用 IPv6，否则回退到 IPv4。只有 IPv4 的 peer 会自动获得 IPv4 端点。

### 每接口独立监听端口

每个 WireGuard peer 都有独立的接口（如 `igp-jp-relay`），各需一个唯一的监听端口。端口从节点的基准端口（`wg_listen_port` 或 `global.wg_listen_port`）开始，按 peer 名字字母序自动分配。

为防止新增/删除节点时端口偏移，可在节点上设置 `wg_peer_port`。这会固定**所有 peer** 用来监听该节点连接的端口：

```yaml
  - name: jp-relay
    wg_peer_port: 60001    # 所有 peer 的 igp-jp-relay 接口监听 60001
```

自动分配会跳过已被 `wg_peer_port` 占用的端口。两个节点不能使用相同的 `wg_peer_port` 值。

对于域名端点，使用 `domain` 字段（WireGuard 会在运行时解析域名）：

```yaml
  - name: us-west
    endpoint:
      domain: "us-west.example.com"
      ipv4: "203.0.113.50"           # 用于 underlay 路由生成
      ipv6: "2001:db8:cafe::1"       # 用于 underlay 路由生成
      ddns: false                    # 预留，未来支持动态 DNS 重解析
```

## Underlay 静态路由

在多宿主主机上，内核可能选错源 IP 发送 WireGuard UDP 包，导致回程流量从错误的接口到达或被 RPF 丢弃。`underlay` 配置会生成带 `krt_prefsrc` 的 BIRD 静态路由来固定源地址。

```yaml
nodes:
  - name: hk-core
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    underlay: {}              # prefsrc 默认用 endpoint IP（1.2.3.4 和 2001:db8::1）

  # 需要覆盖时才显式写 prefsrc
  - name: jp-relay
    endpoint:
      ipv4: "198.51.100.5"
    underlay:
      prefsrc4: "ens3"        # 接口名 — agent 自动选择主 IP
      # 也可以写 "auto" — agent 从默认路由检测
```

`prefsrc` 默认使用节点自己的 endpoint IP。只在需要时覆盖（如接口名或 `"auto"`）。当 `prefsrc` 是接口名时，agent 遵循 Linux 内核的源地址选择规则：主地址优先，跳过过期地址（`preferred_lft 0sec`）。

Agent 在运行时通过 `ip route get` 自动检测默认网关，并写入 `/etc/bird/meshctl-underlay.conf` 的 `protocol static` 段。这些路由直接进入 `master4`/`master6` 安装到内核 FIB，不会重分发进 OSPF。

域名端点需要在 `endpoint` 中显式指定 IP 以生成 underlay 路由：

```yaml
  - name: us-west
    endpoint:
      domain: "us-west.example.com"
      ipv4: "203.0.113.50"
      ipv6: "2001:db8:cafe::1"
      ddns: false               # 预留，未来支持动态 DNS 重解析
    underlay: {}                # prefsrc 默认用 endpoint IP
```

## BIRD 集成

OSPF 路由进入专用表（默认 `igptable4`/`igptable6`，可通过 `global.igp_table4`/`igp_table6` 配置）。运维人员在 `bird.conf` 中用 pipe 连接到 master：

```
# /etc/bird/bird.conf（运维人员管理）
router id 10.200.255.1;
protocol device {}
protocol direct { ipv4; ipv6; }
protocol kernel k4 { ipv4 { export all; }; }
protocol kernel k6 { ipv6 { export all; }; }
protocol static lo4 { ipv4; route 10.200.255.1/32 via "lo"; }

# 把 IGP 表 pipe 到 master
protocol pipe igp4 { table igptable4; peer table master4; import none; export all; }
protocol pipe igp6 { table igptable6; peer table master6; import none; export all; }

include "/etc/bird/meshctl.conf";           # agent 管理（OSPF + 表声明）
include "/etc/bird/meshctl-underlay.conf";  # agent 管理（underlay 静态路由）
include "/etc/bird/bgp.conf";               # 运维人员管理
```

## 隧道寻址

meshctl 根据链路两端的节点类型自动选择寻址模式：

- **双端均为 Linux** — 使用 IPv6 链路本地地址（`fe80::`）+ OSPFv3 AF，无需分配 IP。
- **任一端为非 Linux** — 使用确定性的 `169.254.x.x/31` + OSPFv2，地址由排序后的节点对名称哈希生成，保证相同节点对始终获得相同的 /31。

## 动态开销调优

胖节点之间通过 UDP 探测协议（端口 9473，运行在 WireGuard 隧道内）测量**单向前向延迟**，使用 NTP 同步的时间戳：

```
节点 A                           节点 B
  │── 请求 {seq, T1} ────────→│
  │                             │  T2 = 接收时间
  │                             │  T3 = 发送时间
  │←── 应答 {seq, T1, T2, T3} ─│
T4 = 接收时间

前向延迟 (A→B) = T2 - T1
```

### 为什么用单向延迟而非 RTT

在 Overlay 网络中，A→B 和 B→A 可能经过不同的运营商路径，延迟差异显著。OSPF 开销是按接口方向设定的，节点 A 的 `igp-b` 接口开销应反映 A→B 的前向延迟，而非 RTT 的一半。两个方向的开销可以不同——这正是期望的行为。

### 量化开销分级

前向延迟经 EWMA 平滑后，映射到离散的开销分级，并带有滞回区间以防止 OSPF 振荡：

| 单向延迟 | OSPF 开销 | 下降阈值 |
|---|---|---|
| < 4ms | 20 | — |
| 4–12ms | 50 | 降至 2ms 以下 |
| 12–30ms | 100 | 降至 8ms 以下 |
| 30–60ms | 200 | 降至 20ms 以下 |
| > 60ms | 500 | 降至 40ms 以下 |
| 不可达 | 65535（或 `static_cost` 回退值） | 连续 3 次探测失败 |

分级切换需要连续 5 次探测（hold count）落入新分级后才会生效。

`cost_mode: static` 的节点完全绕过此机制，始终使用配置的 `static_cost`。

### 带宽感知开销

带宽低于 `bandwidth_threshold`（默认 300 Mbps）的链路会获得额外的 OSPF 开销惩罚，使用 Cisco 风格的 auto-cost 公式：

```
link_bw = min(A.bandwidth, B.bandwidth)
惩罚 = reference_bandwidth / link_bw - reference_bandwidth / bandwidth_threshold
```

未显式设置 `bandwidth` 的节点默认等于 `bandwidth_threshold`（无惩罚），确保完全向后兼容。

示例（reference=1000，threshold=300）：100 Mbps 链路 +6 开销，50 Mbps 链路 +16 开销。

## Agent 运行机制

`meshctl-agent` 运行三个独立的循环：

1. **配置同步**（默认 5 分钟） — 从 git/http/本地源按优先级拉取配置，支持回退链和本地缓存
2. **延迟探测**（默认 30 秒） — 对胖节点发送 UDP 时间戳探测，对瘦/静态节点发送 ICMP echo（跳过 `cost_mode: static` 的节点）
3. **开销调整**（每轮探测后） — 当开销分级变化时，重写 BIRD include 文件并触发重新配置

配置拉取失败**不会**阻塞探测和开销调整。Agent 会继续使用缓存的配置运行。反复拉取失败时会启用指数退避（30s → 最大 10 分钟），避免频繁请求 git 服务器。

```bash
meshctl-agent --config /etc/meshctl/agent.yaml
```

Agent 配置示例见 `examples/agent.example.yaml`。

### 监控 Agent 状态

在 `agent.yaml` 中启用 HTTP 健康端点：

```yaml
health_addr: ":9474"
```

然后使用内置的 status 命令查询：

```bash
# 自动从 agent.yaml 读取 health_addr
meshctl-agent status

# 或手动指定地址
meshctl-agent status --addr :9474

# JSON 输出（用于脚本处理）
meshctl-agent status --json
```

输出示例：

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

HTTP 端点也提供原始 JSON：
- `GET /health` — Agent 健康状态（运行时间、配置年龄、NTP 状态）
- `GET /peers` — 逐 peer 延迟、OSPF 开销、分级状态

也可以直接通过 BIRD 验证 OSPF 开销：

```bash
birdc show ospf interface    # 每个接口的当前 OSPF 开销
birdc show ospf neighbors    # 邻居状态
```

## 设计原则

- **无中心节点** — 配置生成是 YAML 的纯函数，可在任意位置运行。各节点独立探测和决策。
- **GitOps 驱动** — 清单和生成的配置存放在私有配置仓库中，胖节点 Agent 按计划拉取。
- **配置生成优于远程控制** — 仅 Linux 胖节点运行 Agent，其他节点接收生成的脚本。
- **稳定优于最优** — 量化分级 + 滞回 + hold count 防止 OSPF 抖动传播到 BGP。

## 项目结构

```
cmd/meshctl/           CLI 入口 (generate, validate, diff, show-mesh, psk)
cmd/meshctl-agent/     Agent 入口 (run, status)
internal/config/       YAML 清单解析与验证
internal/mesh/         链路枚举、模式选择、/31 地址分配
internal/generate/     配置生成器 (BIRD, RouterOS, 静态节点)
internal/cost/         开销分级状态机 (EWMA + 滞回)
internal/probe/        UDP 探测协议与 ICMP 回退
internal/bird/         BIRD 控制套接字客户端
internal/agent/        Agent 运行时 (拉取、应用、探测循环)
examples/              示例配置
scripts/               部署辅助脚本
deployments/           systemd 服务单元
```

## 密钥与 SSH

meshctl 涉及几种不同的密钥，用途各不相同：

| 密钥 | 是什么 | 在哪里 | 用途 |
|---|---|---|---|
| **WireGuard 私钥** | Curve25519 私钥 | 每台节点的 `/etc/meshctl/wireguard.key` | 加密 WireGuard 隧道 |
| **WireGuard 公钥** | 由私钥派生 | `meshctl.yaml` 的 `pubkey` 字段 | 其他节点用来识别本节点 |
| **Deploy key** | Ed25519 SSH 密钥 | 每台节点的 `/etc/meshctl/deploy_key` | Agent 用它 `git fetch` 配置仓库 |
| **PSK 主密钥**（可选） | 32 字节对称密钥 | 每台节点的 `/etc/meshctl/psk-master.key` | 派生链路 PSK，提供后量子保护 |
| **你的 SSH key** | 你自己的 SSH 密钥 | 笔电的 `~/.ssh/` | 你用它 SSH 登录节点做部署 |

### WireGuard 密钥

私钥**不会**出现在 `meshctl.yaml` 中——清单里只保存公钥。`install.sh` 脚本会在远端节点自动生成密钥对并打印公钥。

手动生成：

```bash
./scripts/gen-keys.sh wg-install /etc/meshctl/wireguard.key
# 把打印出的公钥填入 meshctl.yaml 对应节点的 `pubkey` 字段
```

或使用本地初始化脚本：

```bash
./scripts/bootstrap-node.sh
```

### Deploy key（部署密钥）

Deploy key 是一把**只读 SSH 密钥**，让 agent 能从私有配置仓库拉取设定。它不是你个人的 SSH key——是一把权限最小的专用密钥。

```bash
# 在笔电上生成一次
ssh-keygen -t ed25519 -f ~/.ssh/meshctl_deploy_key -N "" -C "meshctl-agent"

# 把公钥添加到配置仓库（GitHub → Settings → Deploy keys，勾选只读）
cat ~/.ssh/meshctl_deploy_key.pub

# install.sh 会自动上传私钥到每台节点
./scripts/install.sh --deploy-key ~/.ssh/meshctl_deploy_key HKG
```

所有胖节点可以共用同一把 deploy key（都是只读拉同一个仓库）。

```
你的笔电                          远端节点                        GitHub/GitLab
  │                                 │                               │
  │── ssh（你的 key）──────────────>│                               │
  │   scp deploy_key ─────────────>│                               │
  │                                 │── git fetch（deploy_key）─────>│
  │                                 │<── mesh-configs 仓库 ─────────│
```

### 预共享密钥 PSK（可选）

PSK 在 WireGuard 的 Curve25519 密钥交换之上再加一层对称加密，提供**后量子保护**——即使 Curve25519 未来被量子计算破解，录制的流量没有 PSK 仍然无法解密。

**需要开启吗？** 多数情况不需要。WireGuard 本身已经足够安全。如果有合规要求或想要纵深防御，可以开启。随时可以后期加入。

工作原理：生成一份主密钥，带外分发到所有胖节点。每个节点用 HKDF-SHA256 独立派生每条链路的 PSK——无需交换。

```bash
# 生成主密钥（只需一次）
./scripts/gen-keys.sh psk-install /etc/meshctl/psk-master.key
# scp 到每台胖节点（绝对不要放进配置仓库）

# 或在部署时用 install.sh 一并上传
./scripts/install.sh --psk /path/to/psk-master.key HKG
```

在清单和 agent 配置中启用：

```yaml
# meshctl.yaml
global:
  psk_enabled: true

# agent.yaml（每台胖节点上）
psk_master_file: "/etc/meshctl/psk-master.key"
```

RouterOS 运维人员手动计算链路 PSK：

```bash
meshctl psk hk-core hk-edge -m /etc/meshctl/psk-master.key
```

## 依赖要求

- 构建：Go 1.22+
- 胖节点：Linux、WireGuard、BIRD 2.x / 3.x、chrony/ntpd
- Agent 需要 `CAP_NET_ADMIN`（WG netlink）+ `CAP_NET_RAW`（ICMP + UDP 探测）
- 瘦节点：RouterOS v7+

## 许可证

见 LICENSE 文件。
