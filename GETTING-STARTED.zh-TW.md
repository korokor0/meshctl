# meshctl 從零開始使用指南

這份文件帶你從完全沒有 mesh 網路開始，一步步建立一套由 meshctl 管理的
WireGuard Overlay Mesh，包含 OSPF 動態成本調整、GitOps 配置分發，以及
Linux 胖節點 + RouterOS 瘦節點的混合部署。

預計整個流程第一次做大約需要 1–2 小時。之後每次新增節點只要修改
一份 YAML 即可。

---

## 0. 你需要準備什麼

### 主機 / 設備

- 至少 **2 台 Linux 胖節點**（Debian 12 / Ubuntu 22.04+ 或其他 systemd 發行版）
- （可選）MikroTik RouterOS v7+ 設備作為瘦節點
- （可選）一台開發機（macOS / Linux 皆可）用來跑 `meshctl` 產生設定

### 權限

- 所有 Linux 節點的 `root`（或 sudo）
- 一個 git 託管平台（GitHub / GitLab / Gitea…）的帳號，用來建立**私有**配置倉庫

### 網路需求

- 每台胖節點都要有一個公網可達的 IP/port（或能被其他節點透過 NAT 打洞抵達）
- 所有胖節點的時鐘必須同步（chrony 或 systemd-timesyncd）——meshctl 用 NTP
  時間戳做單向延遲測量，時鐘誤差會直接反映到 OSPF 成本

### 核心概念（先看這五句話就夠了）

1. **一份 `meshctl.yaml` 就是全部**——所有節點、公鑰、端點、拓樸都在這裡。
2. **兩個 git 倉庫**——code repo（這份 Go 程式）和 config repo（你的 `meshctl.yaml` 和產物），後者是私有的。
3. **兩個二進制檔**——`meshctl`（產生設定，在哪執行都行）和 `meshctl-agent`（只跑在 Linux 胖節點上）。
4. **私鑰永遠不進 YAML**——只有公鑰進 `meshctl.yaml`。私鑰留在該節點本地。
5. **沒有中央控制器**——每個胖節點獨立探測對端、獨立調整自己接口的 OSPF 成本。

---

## 1. 編譯 meshctl

在你的開發機上：

```bash
# 需要 Go 1.22+
git clone https://github.com/honoka/meshctl.git
cd meshctl
make build
```

產物：

```
bin/meshctl               # 設定產生器
bin/meshctl-agent         # 節點 Agent（也是同樣平台的 binary）
```

若要跨編譯給 Linux 節點：

```bash
make release
# 輸出：
#   bin/meshctl-linux-amd64
#   bin/meshctl-agent-linux-amd64
#   bin/meshctl-linux-arm64
#   bin/meshctl-agent-linux-arm64
```

先在本機驗證一下 CLI 能跑：

```bash
./bin/meshctl --help
```

---

## 2. 建立 config 倉庫

這個倉庫跟 meshctl 的程式碼倉庫**完全分開**，而且必須是私有的（裡面會
有公鑰與所有節點的端點資訊）。

```bash
# 在你的 git 平台上建立一個空的私有倉庫，例如 mesh-configs
cd ~
git clone git@github.com:yourorg/mesh-configs.git
cd mesh-configs

# 複製範例清單
cp ~/meshctl/examples/meshctl.example.yaml meshctl.yaml
```

先留著範例檔案，等我們準備好每個節點的公鑰再填。

---

## 3. 準備金鑰與 Deploy Key

meshctl 涉及幾種不同的金鑰：

| 金鑰 | 是什麼 | 在哪裡 | 用途 |
|---|---|---|---|
| **WireGuard 私鑰** | Curve25519 私鑰 | 每台節點的 `/etc/meshctl/wireguard.key` | 加密 WireGuard 隧道 |
| **WireGuard 公鑰** | 由私鑰推導 | `meshctl.yaml` 的 `pubkey` 欄位 | 其他節點用來辨識本節點 |
| **Deploy key** | Ed25519 SSH 金鑰 | 每台節點的 `/etc/meshctl/deploy_key` | Agent 用它 `git fetch` 設定倉庫 |
| **PSK 主密鑰**（可選） | 32 bytes 對稱金鑰 | 每台節點的 `/etc/meshctl/psk-master.key` | 推導鏈路 PSK，提供後量子保護 |
| **你的 SSH key** | 你自己的 SSH 金鑰 | 筆電的 `~/.ssh/` | 你用它 SSH 進節點做部署 |

### 3.1 產生 Deploy Key（只做一次）

Deploy key 是一把**唯讀 SSH 金鑰**，專門讓 agent 從你的私有 config repo
做 `git fetch`。它**不是**你個人的 SSH key——是一把權限最小的專用金鑰。

```bash
# 在你的筆電上產生
ssh-keygen -t ed25519 -f ~/.ssh/meshctl_deploy_key -N "" -C "meshctl-agent"

# 把公鑰加到 config repo（GitHub → Settings → Deploy keys，勾選 read-only）
cat ~/.ssh/meshctl_deploy_key.pub
```

所有胖節點可以**共用同一把** deploy key（都是唯讀拉同一個 repo），所以
只需產生一次。

金鑰流向：

```
你的筆電                          遠端節點                        GitHub/GitLab
  │                                 │                               │
  │── ssh（你的 key）──────────────>│                               │
  │   scp deploy_key ─────────────>│                               │
  │                                 │── git fetch（deploy_key）─────>│
  │                                 │<── mesh-configs repo ─────────│
```

### 3.2 為胖節點產生 WireGuard 金鑰

有兩種方式：

**方式 A：用 `install.sh` 一步搞定**（推薦，見第 6 節）

`install.sh` 會在遠端節點上自動建立目錄、產生 WireGuard 金鑰、上傳
deploy key 和 agent，最後印出公鑰讓你填進 `meshctl.yaml`。如果你打算用
`install.sh`，可以跳到第 4 節，金鑰的事第 6 節會自動處理。

**方式 B：手動在節點上操作**

在每台 Linux 胖節點上：

```bash
# 安裝必要工具
apt install -y wireguard-tools bird2 chrony

# 確認時鐘同步
chronyc tracking | grep "Leap status"   # 應該是 "Normal"

# 用 bootstrap 腳本一鍵初始化（建目錄 + 生金鑰）
# 把 bootstrap-node.sh scp 過去或直接下載
./scripts/bootstrap-node.sh
```

腳本會建立 `/etc/meshctl/`、產生私鑰（`wireguard.key`，mode 0600），
並印出公鑰。把公鑰記下來。

或者不用腳本，手動做：

```bash
mkdir -p /etc/meshctl/cache
chmod 700 /etc/meshctl
wg genkey | tee /etc/meshctl/wireguard.key | wg pubkey
chmod 600 /etc/meshctl/wireguard.key
```

上面最後那行**印出來的就是公鑰**，例如：

```
aB3dEf9GhIjKlMnOpQrStUvWxYz0123456789abcdefg=
```

### 3.3 RouterOS 節點的金鑰

RouterOS 比較特別：必須先在 RouterOS 上建立 WG 介面讓它自己產生金鑰，
再把公鑰抄回來。這一步我們放到第 7 節處理。現在先跳過。

---

## 4. 撰寫 `meshctl.yaml`

回到你的開發機，在 `mesh-configs/` 目錄下編輯 `meshctl.yaml`。

假設你有三台節點：

| 節點 | 類型 | 位置 | 公網端點 |
|---|---|---|---|
| `hk-core` | linux（胖） | 香港 | `[2001:db8::1]:51820` |
| `jp-relay` | linux（胖） | 東京 | `198.51.100.5:51820` |
| `hk-edge` | routeros（瘦） | 香港 | `[2001:db8::2]:13231` |

最小可用的清單長這樣：

```yaml
global:
  wg_listen_port: 51820
  probe_interval: 30s

nodes:
  # node_id: 確定性地址分配。V4LL = base + node_id, fe80 = fe80::127:<node_id>。
  # 省略時按字母序自動分配。顯式設定可防止增刪節點時地址飄移。

  # 雙棧節點：同時寫上 v4 和 v6，生成器會根據對端能力選擇
  - name: hk-core
    type: linux
    node_id: 1                  # → 169.254.0.1, fe80::127:1
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    loopback: 10.200.255.1
    announce:
      - 192.168.1.0/24        # 這個節點後面的區網，OSPF 會通告出去
    pubkey: "aB3d...="          # 貼上 hk-core 本機產生的公鑰
    underlay: {}                # prefsrc 預設用 endpoint IP，不需重複寫

  # 只有 IPv4 的節點
  - name: jp-relay
    type: linux
    node_id: 3                  # → 169.254.0.3, fe80::127:3
    endpoint:
      ipv4: "198.51.100.5"
    loopback: 10.200.255.3
    pubkey: "cD5f...="          # 貼上 jp-relay 本機產生的公鑰
    underlay: {}                # prefsrc4 預設用 198.51.100.5

  - name: hk-edge
    type: routeros
    node_id: 2                  # → 169.254.0.2, fe80::127:2
    endpoint:
      ipv6: "2001:db8::2"
    loopback: 10.200.255.2
    announce:
      - 192.168.2.0/24
    pubkey: "PLACEHOLDER_WILL_FILL_LATER="  # 第 7 節會回來填
    wg_listen_port: 13231
    wg_peer_port: 60001         # 固定端口：所有 peer 的 igp-hk-edge 介面監聽 60001
    # 瘦節點使用靜態 OSPF 成本，不做 ICMP 探測
    cost_mode: static
    static_cost: 150

link_policy:
  mode: full                  # 全互聯 mesh
```

### 欄位快速說明

- `node_id`: 確定性地址編號。V4LL 地址 = base + node_id（如 `169.254.0.2`），fe80 = `fe80::127:<node_id>`。省略時按名字字母序自動分配。建議顯式設定以避免增刪節點時地址飄移
- `loopback`: 該節點唯一的路由器 ID，每個節點手動指定一個 /32
- `announce`: 該節點後面想通告進 mesh 的內網網段（OSPF 會廣播）
- `pubkey`: WG 公鑰，**只有**公鑰
- `endpoint`: struct 格式，填 `ipv4`、`ipv6`，雙棧節點兩個都填；域名節點填 `domain`，DDNS 填 `ddns: true`。Port 來自 `wg_listen_port` 或 `global.wg_listen_port`，不需要寫在端點裡。生成器根據對端能力選協議（優先 v6）。**NAT 後無公網 IP 的節點可省略 `endpoint`**——該節點主動連線，對端等待連入。每條鏈路至少一端必須有 endpoint，否則 `generate` 會報錯
- `wg_peer_port`: 固定端口號，所有 peer 的 `igp-<此節點>` 介面監聯此端口。避免新增節點後端口偏移
- `bandwidth`: 節點帶寬（Mbps）。低於 `bandwidth_threshold`（預設 300）的鏈路會按比例增加 OSPF 成本（乘法加權，penalty=20 → +20%）。省略則預設等於閾值（無懲罰）
- `cost_mode`: `probe`（預設，ICMP rtt/2 動態）或 `static`（固定成本）
- `underlay`: 固定 underlay 流量的源 IP（見第 9 節）
- 更完整的範例見 `examples/meshctl.example.yaml`

### 驗證與預覽

```bash
cd ~/meshctl
./bin/meshctl validate --config ~/mesh-configs/meshctl.yaml
./bin/meshctl show-mesh --config ~/mesh-configs/meshctl.yaml
```

`show-mesh` 會列出所有節點、所有鏈路、每條鏈路用的定址模式（`fe80`
還是 `v4ll`），幫你檢查拓樸是不是預期的樣子。

---

## 5. 產生設定檔

```bash
cd ~/mesh-configs
~/meshctl/bin/meshctl generate --config meshctl.yaml
```

會在 `output/` 下建立每個節點的子目錄：

```
output/
├── hk-core/
│   ├── bird-meshctl.conf      # BIRD OSPF 設定
│   └── wireguard.json         # 給 agent 用的 WG peers 描述
├── jp-relay/
│   ├── bird-meshctl.conf
│   └── wireguard.json
└── hk-edge/
    ├── wireguard.rsc
    ├── ospf.rsc
    ├── addresses.rsc
    ├── full-setup.rsc
    └── README.txt
```

提交到 config repo：

```bash
git add meshctl.yaml output/
git commit -m "initial mesh configuration"
git push
```

---

## 6. 部署 Agent 到 Linux 胖節點

每台 Linux 胖節點上都要做以下步驟。下面以 `hk-core` 為例。

### 6.1 設定 BIRD

先 SSH 到節點，編輯 `/etc/bird/bird.conf`。meshctl 的 OSPF 路由會放進
獨立的表（預設 `igptable4`/`igptable6`），你需要在 `bird.conf` 裡用
pipe 把它們連到 master：

```
# /etc/bird/bird.conf
router id 10.200.255.1;

protocol device {}
protocol direct { ipv4; ipv6; }
protocol kernel k4 { ipv4 { export all; }; }
protocol kernel k6 { ipv6 { export all; }; }
protocol static lo4 { ipv4; route 10.200.255.1/32 via "lo"; }

# 把 IGP 表 pipe 到 master（表名可在 meshctl.yaml 的 global 區塊自訂）
protocol pipe igp4 { table igptable4; peer table master4; import none; export all; }
protocol pipe igp6 { table igptable6; peer table master6; import none; export all; }

include "/etc/bird/meshctl.conf";           # agent 管理（OSPF + 表宣告）
include "/etc/bird/meshctl-underlay.conf";  # agent 管理（underlay 靜態路由）
# include "/etc/bird/bgp.conf";            # 你自己的 BGP 設定
```

支援 BIRD 2.x 和 3.x，設定語法一樣。`meshctl.conf` 和
`meshctl-underlay.conf` 不用你自己建立，Agent 拉到設定後會自動寫入。

> **為什麼分開表？** OSPF 路由進 `igptable4`/`igptable6`，underlay 靜態
> 路由直接進 `master4`/`master6`（需要 `krt_prefsrc` 安裝到內核 FIB）。
> 分開的 IGP 表方便你在 pipe 上加 filter 或做路由策略。表名可透過
> `meshctl.yaml` 的 `global.igp_table4`/`igp_table6` 自訂。

### 6.2 用 install.sh 一鍵部署（推薦）

```bash
# 在你的開發機上
cd ~/meshctl

# 先交叉編譯
make release

# 設定環境變數（只需做一次，之後每台節點直接用）
export MESHCTL_REPO_URL="git@github.com:yourorg/mesh-configs.git"
export MESHCTL_DEPLOY_KEY="~/.ssh/meshctl_deploy_key"

# 一條命令部署到 hk-core（HKG 是 ~/.ssh/config 的別名）
./scripts/install.sh HKG

# 或指定 node name
./scripts/install.sh --node hk-core root@hk-core.example.com

# 如果要同時安裝 PSK
./scripts/install.sh --psk /path/to/psk-master.key HKG
```

腳本會自動：

1. 偵測遠端架構（amd64/arm64），上傳正確的 binary
2. 建立 `/etc/meshctl/` 和 `/etc/meshctl/cache/`
3. 在遠端產生 WireGuard 金鑰對（已存在則跳過）
4. 上傳 deploy key 到 `/etc/meshctl/deploy_key`
5. 產生 `/etc/meshctl/agent.yaml`
6. 安裝 systemd unit，`enable --now`
7. **印出公鑰**——把它貼進 `meshctl.yaml`

輸出範例：

```
Public key (add to meshctl.yaml):

  - name: hkg
    type: linux
    pubkey: "aB3dEf9GhIjKlMnOpQrStUvWxYz0123456789abcdefg="
```

### 6.3 手動部署（如果你不想用腳本）

```bash
# 1. 拷貝 binary
scp bin/meshctl-agent-linux-amd64 root@hk-core:/usr/local/bin/meshctl-agent

# 2. 拷貝 systemd unit
scp deployments/meshctl-agent.service root@hk-core:/etc/systemd/system/

# 3. 初始化節點（建目錄 + 生金鑰）
scp scripts/bootstrap-node.sh root@hk-core:/tmp/
ssh root@hk-core '/tmp/bootstrap-node.sh'

# 4. 上傳 deploy key
scp ~/.ssh/meshctl_deploy_key root@hk-core:/etc/meshctl/deploy_key
ssh root@hk-core 'chmod 600 /etc/meshctl/deploy_key'

# 5. 在節點上建立 agent 設定
ssh root@hk-core 'cat > /etc/meshctl/agent.yaml' <<'EOF'
node_name: hk-core

repo:
  sources:
    - type: git
      url: "git@github.com:yourorg/mesh-configs.git"
      branch: main
      ssh_key: "/etc/meshctl/deploy_key"
  fetch_timeout: 30s
  local_cache: "/etc/meshctl/cache/"

config_sync_interval: 5m
probe_interval: 30s
probe_port: 9473
bird_socket: "/var/run/bird/bird.ctl"
bird_include_path: "/etc/bird/meshctl.conf"

private_key_file: "/etc/meshctl/wireguard.key"
# psk_master_file: "/etc/meshctl/psk-master.key"   # 第 8 節會用到

health_addr: ":9474"   # 啟用 HTTP 健康端點（查看延遲和 OSPF 開銷用）
EOF

# 6. 啟動
ssh root@hk-core 'systemctl daemon-reload && systemctl enable --now meshctl-agent'
```

### 6.4 確認 Agent 正常運作

```bash
ssh root@hk-core 'journalctl -u meshctl-agent -f'
```

你應該會看到類似這樣的訊息：

```
starting probe server port=9473
starting config sync
creating WG interface interface=igp-jp-relay
BIRD config updated, reconfiguring
config sync completed
```

然後用標準工具檢查：

```bash
wg show                             # WG 介面是否都起來了
ip route | grep 10.200.255          # loopback 路由是否學到
birdc show protocols                # OSPF 鄰居是否 Full
birdc show route                    # 是否收到對端的 announce
```

等探測跑幾輪（預設 30 秒一輪）後，可以用 `status` 子命令查看逐 peer 的
延遲和 OSPF 開銷：

```bash
meshctl-agent status                # 如果 agent.yaml 有設 health_addr
meshctl-agent status --addr :9474   # 或直接指定地址
```

輸出示例：

```
Node:        hk-core
Uptime:      5m30s
Config age:  4m12s
Last probe:  28s ago
Peers:       2
NTP synced:  yes  (offset 1.2ms)

PEER         INTERFACE    FORWARD  RTT      BAND  COST  MODE   STATUS
jp-relay     igp-jp-rel   3.2ms    6.1ms    0     20    probe  ok
```

也可以用 `--json` 輸出 JSON 格式，方便腳本處理。

重複同樣步驟到其他 Linux 胖節點（`jp-relay` …）。

---

## 7. 加入 RouterOS 瘦節點

RouterOS 不能跑 Agent，所以要走兩階段：**先上一次設定讓它產生 WG 金鑰，
抄回公鑰填進 `meshctl.yaml`，再重新產生並上第二次設定。**

### 7.1 第一次匯入（先建立 WG 介面）

```bash
# 把產生好的 .rsc 檔傳到 RouterOS
scp output/hk-edge/full-setup.rsc admin@192.168.88.1:/

# 登入 RouterOS 執行匯入
ssh admin@192.168.88.1
/import full-setup.rsc
```

### 7.2 把 RouterOS 的公鑰抄回來

```
[admin@hk-edge] > /interface wireguard print detail
```

輸出中會有一行 `public-key="..."`。把它複製下來，填回到 `meshctl.yaml`
裡 `hk-edge` 節點的 `pubkey` 欄位，取代之前的 placeholder。

### 7.3 重新產生並重新匯入

```bash
cd ~/mesh-configs
~/meshctl/bin/meshctl generate --config meshctl.yaml
git add output/ meshctl.yaml
git commit -m "fill hk-edge pubkey"
git push

# 重新匯入 .rsc（idempotent，會更新 peers 而不是重建）
scp output/hk-edge/full-setup.rsc admin@192.168.88.1:/
ssh admin@192.168.88.1 '/import full-setup.rsc'
```

其他胖節點的 Agent 下一次 config sync（預設 5 分鐘）就會自動拉到新的
`meshctl.yaml` 並加入 hk-edge 作為 WG peer。如果等不及，可以手動觸發：

```bash
ssh root@hk-core 'systemctl kill -s HUP meshctl-agent'
```

---

## 8. （可選）啟用預共享金鑰（PSK）

### PSK 是什麼？

PSK（Pre-Shared Key，預共享金鑰）是 WireGuard 的一層**額外對稱加密**。

WireGuard 已經用 Curve25519 做金鑰交換，安全性足夠。但如果未來量子電腦
能破解橢圓曲線，被錄下的流量就能被回溯解密。加了 PSK 後，在 Curve25519
之上再混入一把對稱金鑰（32 bytes），即使公鑰加密被破，沒有 PSK 仍然
解不開——這就是所謂的**後量子防護層**。

**要不要開？**

| 情境 | 建議 |
|---|---|
| 一般內部 mesh | 不需要，WireGuard 本身已經很安全 |
| 有合規要求、或在意量子安全 | 開啟 |
| 懶得多管一把金鑰 | 不開，之後要加隨時可以 |

簡單說：**可選的額外保險，不影響功能，不開也完全沒問題。**

### 在 meshctl 裡怎麼運作

meshctl 用 HKDF-SHA256 從**一份共享主密鑰**推導出每條鏈路的獨立 PSK，
兩端用相同算法各自計算，結果完全一致，不需要交換任何東西：

```
PSK master（一把，所有節點共享）
       │
       │  HKDF-SHA256("meshctl-psk-v1|hk-core|jp-relay")
       ▼
  per-link PSK（每條隧道各自不同）
```

### 8.1 產生並分發主密鑰

**只在一台機器上**產生一次：

```bash
./scripts/gen-keys.sh psk-install /etc/meshctl/psk-master.key
```

然後用帶外管道（`scp` / Ansible / 手動貼）把**這個檔案**複製到**每一台**
胖節點的同樣位置。**不要**放進 config repo。

```bash
scp /etc/meshctl/psk-master.key root@jp-relay:/etc/meshctl/psk-master.key
# ...每台胖節點都要
```

或者在部署新節點時用 `install.sh` 一併上傳：

```bash
./scripts/install.sh --psk /path/to/psk-master.key HKG
```

### 8.2 開啟 PSK

在 `meshctl.yaml` 的 `global` 區塊加上：

```yaml
global:
  # ...其他欄位...
  psk_enabled: true
```

重新產生與提交：

```bash
~/meshctl/bin/meshctl generate --config meshctl.yaml
git add -A && git commit -m "enable psk" && git push
```

在每台胖節點的 `/etc/meshctl/agent.yaml` 裡把這行取消註解：

```yaml
psk_master_file: "/etc/meshctl/psk-master.key"
```

重啟 agent：

```bash
systemctl restart meshctl-agent
```

Agent 下一次 apply 時會自動為每條鏈路派生 PSK 並透過 `wg set peer
<pk> preshared-key <tmpfile>` 應用到內核。

### 8.3 RouterOS 節點的 PSK

RouterOS 不會自己派生，需要你手動計算每條涉及 RouterOS 的鏈路的 PSK，
貼進產生出來的 `.rsc`：

```bash
# 在你的開發機上（開發機也要能讀到 psk-master.key）
~/meshctl/bin/meshctl psk hk-core hk-edge -m /path/to/psk-master.key
# 輸出：fKtP0Zj+npIu9KFK7jLQDKWoHUzmJDTPiXHIiyMW+pA=
```

把這個字串貼進 `output/hk-edge/full-setup.rsc` 中對應 peer 的
`preshared-key` 欄位，然後重新匯入。

---

## 9. （可選）Underlay 靜態路由

如果你的胖節點是多宿主（多張網卡或多個 IP），內核可能在發 WireGuard
UDP 包時選錯源 IP，導致回程流量走錯接口或被 RPF 丟棄。`underlay` 設定
讓 agent 自動產生帶 `krt_prefsrc` 的 BIRD 靜態路由來固定源地址。

### 9.1 在 meshctl.yaml 加上 underlay

最簡單的寫法——`prefsrc` 預設用你的 endpoint IP：

```yaml
nodes:
  - name: hk-core
    endpoint:
      ipv4: "1.2.3.4"
      ipv6: "2001:db8::1"
    underlay: {}        # prefsrc6=2001:db8::1, prefsrc4=1.2.3.4（自動）
```

需要覆蓋時才顯式寫 `prefsrc`：

```yaml
  - name: jp-relay
    endpoint:
      ipv4: "198.51.100.5"
    underlay:
      prefsrc4: "ens3"  # 用介面名代替 endpoint IP
```

`prefsrc` 接受四種寫法：

| 寫法 | 行為 |
|---|---|
| 留空（預設） | 使用本節點的 endpoint IP |
| IP 地址（如 `"2001:db8::1"`） | 直接用作 `krt_prefsrc` |
| 介面名（如 `"ens3"`） | Agent 取該介面的主 IP，遵循 Linux 內核選址規則：主地址優先，跳過 `preferred_lft 0sec` 的過期地址 |
| `"auto"` | Agent 先查默認路由的 `src`，找不到再用 `ip route get` 讓內核選 |

### 9.2 域名端點的情況

如果節點的 endpoint 使用域名，在 `endpoint` 結構中同時提供 IP 以便生成 underlay 路由：

```yaml
  - name: us-west
    endpoint:
      domain: "us-west.example.com"
      ipv4: "203.0.113.50"
      ipv6: "2001:db8:cafe::1"
      ddns: false               # 預留：true 時 agent 會定期重解析
    underlay: {}                # prefsrc 預設用 endpoint IP
```

域名端點會原樣傳給 WireGuard（WG 自己解析）。`endpoint` 結構中的 `ipv4`/`ipv6` 用於端點選擇和產生 underlay 路由。

### 9.3 Agent 做了什麼

1. 讀取 `wireguard.json` 裡的 `underlay_routes`
2. 解析每條路由的 `prefsrc`（IP / 介面名 / auto）為實際 IP
3. 用 `ip route get <dest>` 偵測當前默認閘道
4. 寫入 `/etc/bird/meshctl-underlay.conf`（`protocol static` 段）
5. 這些路由進 `master4`/`master6`（直接安裝到內核 FIB），不會重分發進 OSPF

不需要手動操作——Agent 會在每次 config sync 時自動處理。

---

## 10. 驗證整個 mesh 是否正常

在任意一台胖節點上：

```bash
# 1. WG 鄰居握手成功
wg show
# 每個 peer 的 latest handshake 應該在 2 分鐘內

# 2. OSPF 鄰居達到 Full
birdc show ospf neighbors
birdc show ospf neighbors 'meshctl_ospf3'

# 3. loopback 路由互通
ping -c3 10.200.255.3     # 從 hk-core ping jp-relay 的 loopback

# 4. announce 的內網可達
ping -c3 192.168.2.1      # ping hk-edge 後面的內網

# 5. meshctl-agent 日誌沒有 error
journalctl -u meshctl-agent --since "5 minutes ago" | grep -iE 'error|warn'
```

### 檢查動態成本是否生效

探測結果會記錄在 agent 日誌裡。當延遲變化跨越了 band 閾值，你會看到：

```
cost band changed peer=jp-relay forward_delay=15.3ms cost=100
```

此時 `birdc show ospf interface` 應該會顯示新的 cost。

---

## 11. 日常操作

### 新增節點

```bash
cd ~/mesh-configs
vim meshctl.yaml              # 加一筆 node
~/meshctl/bin/meshctl diff    # 預覽變更
~/meshctl/bin/meshctl generate
git add -A && git commit -m "add sg-relay" && git push

# 現有的胖節點 5 分鐘內會自動拉到新設定
# 新節點的部署依照第 3 + 6 節流程
```

### 移除節點

```bash
vim meshctl.yaml              # 刪掉該節點區塊
~/meshctl/bin/meshctl generate
git add -A && git commit -m "remove sg-relay" && git push

# 其他節點會自動移除對應的 WG peer
# 在被移除的節點本機：
systemctl disable --now meshctl-agent
ip link delete igp-<peer>     # 手動清理殘留的 WG 介面
```

### 更新 `meshctl-agent` binary

Binary 不會自動升級（它從 code repo 來，不是 config repo）。需要時：

```bash
make release
./scripts/install.sh upgrade HKG              # 替換 + 重啟
./scripts/install.sh upgrade --no-restart HKG # 只替換，之後手動重啟

# 批量升級多台節點（預設最多 4 個並行）
./scripts/install.sh upgrade HKG TYO LAX

# 滾動升級——逐台執行，遇到失敗立刻中止
./scripts/install.sh upgrade --rolling HKG TYO LAX
```

`install.sh upgrade` 不動金鑰、設定和 systemd unit。加 `--no-restart`
可以先把所有節點的 binary 都換好，再統一重啟。批量模式結束後會印出
成功/失敗摘要。

也可以手動操作：

```bash
scp bin/meshctl-agent-linux-amd64 root@hk-core:/usr/local/bin/meshctl-agent
ssh root@hk-core 'systemctl restart meshctl-agent'
```

### 手動觸發 config sync

```bash
systemctl kill -s HUP meshctl-agent
```

### 本機備援（所有 git 來源都掛掉時）

Agent 的多來源優先級支援 `local` 類型；把手動產生的檔案丟到節點上某
個路徑，在 agent.yaml 的 `repo.sources` 最後加一條：

```yaml
    - type: local
      path: "/etc/meshctl/manual-configs/"
```

即使所有 git 都掛了，agent 還會用這個目錄裡的設定繼續跑，而且探測和
成本調整迴圈**永遠**不會被 fetch 失敗阻塞。

---

## 12. 疑難排解

### WG 介面建立成功但握手失敗

- 檢查公網端點是否可達：`nc -u -vz <endpoint> <port>`
- 檢查防火牆：`ufw status` / `iptables -L` — WG 預設 port 51820/udp 必須放行
- 兩端的 `pubkey` 是否對得上（最常見的錯誤：貼錯節點的公鑰）

### OSPF 鄰居卡在 ExStart / Init

- 兩端的 `wg` 介面是否都有 link-local 或 /31 地址：`ip addr show igp-*`
- `wg show` 的 `latest handshake` 是否有值
- `birdc show ospf neighbors` 看具體狀態，配合 `birdc debug`

### Agent 不停重啟

```bash
journalctl -u meshctl-agent -n 100
```

最常見的原因：

- `/etc/meshctl/wireguard.key` 不存在或權限不對
- deploy key 沒裝好，git clone 失敗而且沒有 local cache
- BIRD socket 路徑錯誤（`bird_socket`）

### 時鐘不同步導致 OSPF 成本亂跳

```bash
chronyc tracking
```

如果 offset > 10ms，就會影響單向延遲測量的準確度。Agent 會把負值
measurement 丟掉，但漂移還是會反映到 band 切換上。修好 NTP 即可。

### PSK 啟用後兩端派生的 key 對不上

- 確認兩台節點的 `/etc/meshctl/psk-master.key` **位元完全一致**
  （`sha256sum` 比對）
- 確認 `meshctl.yaml` 的 `global.psk_enabled: true` 而且 agent 已經重啟
- `wg show` 的 peer 區塊應該顯示 `preshared key: (hidden)`

---

## 13. 下一步

- 讀 `CLAUDE.md` 了解設計決策與內部架構
- 讀 `README.md` / `README.zh.md` 看 cost band、成本模式等細節
- 為你的場景客製 `cost_bands`、`probe_interval`、`ewma_alpha`
- 把 config repo 接上 CI，在 `meshctl.yaml` 變更時自動跑 `meshctl generate`

到此為止，你已經有一個可以自我維護、低延遲路由自動優化、配置漂亮乾淨
的 Overlay Mesh 了。歡迎加入節點。
