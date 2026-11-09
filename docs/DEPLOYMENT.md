# ATL-MCPanel 部署指南

面向单机 / 多节点生产部署。全部组件均为 Go 单个二进制 + systemd 托管，无外部依赖（数据库为内嵌 SQLite）。

---

## 1. 架构与部署形态

```
                   公网 / 玩家
                        │
             ┌──────────┴───────────┐
             │   frps（公网服务器）  │  ← 可选：内网穿透
             └──────────┬───────────┘
                        │ frp 隧道
        ┌───────────────┴────────────────┐
        │  Panel（管理面板，单实例）      │
        │  :8080 HTTP  :8443 HTTPS       │
        │  :9090 gRPC（mTLS，Daemon 接入）│
        └───────────────┬────────────────┘
                        │ mTLS
        ┌───────────────┴────────────────┐
        │  Daemon（每台实例机器一个）      │
        │  :9091 gRPC（mTLS）             │
        │  管理 Minecraft 进程            │
        └────────────────────────────────┘
```

| 组件 | 部署数量 | 说明 |
|------|---------|------|
| Panel | 1（可多但需自行处理数据库共享） | 提供 Web 界面与 API |
| Daemon | 每个运行实例的机器 1 个 | 实际启动/停止/备份 Minecraft |
| frps | 0 或 1 | 无公网 IP 时用于对外暴露游戏端口与面板 |

---

## 2. 环境要求

- **Linux**（已在 Debian 13 验证），systemd
- **gcc**：SQLite 驱动依赖 CGO，若自行编译需要；使用发布包则不需要
- 端口：面板 8080/8443/9090；节点 9091；游戏端口与 frp 端口按需
- 建议 2 核 2GB 以上（面板本身很轻，主要取决于实例数量）

---

## 3. 安装面板

### 3.1 从发布包安装（推荐）

```bash
# 在开发机构建发布包
./scripts/build-release.sh --version 1.0.0

# 传输并安装
scp dist/atlmcpanel-1.0.0-linux-amd64.tar.gz root@<面板IP>:/tmp/
ssh root@<面板IP>
cd /tmp && tar xzf atlmcpanel-1.0.0-linux-amd64.tar.gz
cd atlmcpanel-1.0.0-linux-amd64
./deploy/install.sh panel                 # 默认安装到 /opt/mcpanel
```

安装脚本会：
1. 创建 `/opt/mcpanel/{bin,data,instances,certs,logs}`
2. 创建一个**专用系统用户**（默认 `atlpanel`）并把 `data/`、`certs/`、`logs/` 交给它
3. 安装二进制与前端静态资源
4. 生成初始 `config.yaml`（**已存在则保留，便于升级**）
5. 生成自签 HTTPS 证书（若安装了 openssl）
6. 安装并启动 `atlmcpanel-panel.service`（以 `atlpanel` 身份运行，带 systemd 沙箱）

> 脚本是**幂等**的：升级时重新解压并再次执行即可，配置与数据不受影响。

#### 面板运行用户与沙箱（0.11.3 起）

```bash
sudo ./deploy/install.sh panel                  # 默认：以 atlpanel 身份运行 + 全量沙箱
sudo PANEL_USER=myuser ./deploy/install.sh panel # 换成自己指定的用户
sudo PANEL_USER=root ./deploy/install.sh panel   # 退回老行为（面板以 root 跑，不加沙箱）
```

- 面板有权限写的只有 `data/` 与 `certs/`；`config.yaml` 是 `root:atlpanel 0640`（只读）；
- 代价：**「本机一键部署节点」在非 root 下不可用**（要写 `/etc/systemd/system`）。
  节点页会直接说明原因 —— 在节点机器上跑 `deploy/install.sh daemon`，或走 SSH / 凭证交换 / 粘贴导入登记；
- 逐项加固矩阵、以及「为什么节点的单元不能加同样的项」见 `deploy/systemd/HARDENING.md`。

#### 容器化安装（可选）

```bash
./deploy/docker/build.sh --tag atl-mcpanel-panel:0.11.3
sudo mkdir -p /srv/mcpanel/{data,certs}
sudo cp config.example.yaml /srv/mcpanel/config.yaml
sudo chown -R 10001:10001 /srv/mcpanel/data /srv/mcpanel/certs /srv/mcpanel/config.yaml
sudo chmod 640 /srv/mcpanel/config.yaml
ATL_ROOT=/srv/mcpanel ATL_CERTS=/srv/mcpanel/certs ATL_CONFIG=/srv/mcpanel/config.yaml \
  docker compose -f deploy/docker/docker-compose.yml up -d
```

容器版与节点**同机时必须用 host 网络**（节点的 `atl-container-firewall` 会挡容器→宿主的新连接），
详见 `deploy/docker/README.md`。「本机一键部署」同样不可用（容器里没有 systemd）。

### 3.2 从源码安装

```bash
git clone https://github.com/ATLCNND/ATL-MCPanel.git
cd ATL-MCPanel
sudo apt-get install -y golang gcc nodejs npm
bash scripts/build.sh
cd web && npm install && npm run build && cd ..
sudo ./deploy/install.sh panel
```

### 3.3 首次登录

浏览器打开 `http://<面板IP>:8080`，**注册的第一个账号自动成为管理员**。

> 安全提示：注册接口在存在用户后即要求管理员身份，因此不会出现"任何人都能注册"的情况。
> 但首个账号建议尽快创建，避免被他人抢先。

---

## 4. 安装节点（Daemon）

### 方式 A：面板一键部署（推荐）

1. 面板 → **节点管理** → **+ 登记节点**，填写节点名称、IP、SSH 账号密码（或私钥内容）
2. 点击 **环境探测**，确认 systemd 可用、磁盘充足
3. 点击 **一键部署**

面板会自动完成：
```
签发 mTLS 证书 → 上传 Daemon 二进制 → 上传证书 → 写入配置
→ 安装 systemd 单元 → 启动服务 → 校验运行状态
```

部署后节点会出现在列表中并显示为 **online**。

### 方式 B：手工安装

```bash
# 在节点上解压发布包
cd /tmp && tar xzf atlmcpanel-1.0.0-linux-amd64.tar.gz
cd atlmcpanel-1.0.0-linux-amd64
sudo ./deploy/install.sh daemon
```

然后手工配置：

1. 在面板 **节点管理 → 导出证书** 取得 CA 证书与节点证书
2. 写入节点文件：

```bash
mkdir -p /opt/mcpanel/certs
# 将导出内容分别写入以下文件
cat > /opt/mcpanel/certs/ca.crt   <<'EOF'
...
EOF
cat > /opt/mcpanel/certs/node.crt <<'EOF'
...
EOF
cat > /opt/mcpanel/certs/node.key <<'EOF'
...
EOF
chmod 600 /opt/mcpanel/certs/node.key
```

3. 修改 `/opt/mcpanel/config.yaml`：

```yaml
daemon:
  node_id: "node-002"                  # 必须与签发证书时使用的名称一致
  panel_address: "10.0.0.10:9090"      # 面板 gRPC 地址（示例；按你的实际地址填）
  panel_url: ""                        # 只有「节点发起」接入需要（面板 HTTP 地址，如 http://10.0.0.10:8080）
  grpc_listen: ":9091"
  instance_dir: "instances"
  tls: true
  cert_file: "/opt/mcpanel/certs/node.crt"
  key_file: "/opt/mcpanel/certs/node.key"
  ca_file: "/opt/mcpanel/certs/ca.crt"
```

4. 重启：`systemctl restart atlmcpanel-daemon`

日志出现 `注册成功` 即接入完成。

#### 用「节点发起」接入（`enroll --init`）

节点上 `atl-daemon enroll --init` 会打印一行 `atl-join-v1.<…>` 接入请求，管理员把它贴进
面板（登记节点 → 粘贴导入）后**节点会自己把证书领回来** —— 前提是 `config.yaml` 里填了
`daemon.panel_url`（面板的 **HTTP** 地址；`panel_address` 是 gRPC 地址，不能用它领证书）：

```yaml
daemon:
  node_id: "node-002"
  panel_address: "10.0.0.10:9090"
  panel_url: "http://10.0.0.10:8080"   # ← 节点发起接入必须有
```

启动后节点在 3 分钟内每 5 秒向面板要一次证书（管理员贴请求慢一点也没关系），
日志依次出现 `已从面板领回节点证书` → `注册成功`。没导入时日志是
`面板还没有导入这台节点的接入请求，稍后重试`（不退出，等你自己贴）。
`node_id` 必须与接入请求里的节点名一致 —— 面板按**证书 CN** 校验身份，用别的名字会报
「节点身份校验失败」。

#### 证书续期（不用管，但知道一下）

节点证书有效期 **30 天**。Daemon 启动后先查一次、之后每 6 小时查一次，
**剩余不足 7 天就自动续期**：用本机 `certs/node.key` 造 CSR 打面板
`POST /api/enroll/renew`，新证书原子写回 `node.crt` / `ca.crt`，私钥不动；
TLS 证书在每次握手前按文件状态热加载，所以**续期后无需重启**（重启 Daemon 会杀掉实例，
生产环境尤其不能随便重启）。面板地址优先取 `daemon.panel_url`，缺失时退回接入时记下的
`state/panel_url`；两者都没有时日志会明确报「节点证书将于 … 到期，但没有面板 HTTP 地址」。

续期失败只会在日志里出现（不影响当前运行，6 小时后再试），常见两种：

| 日志 | 含义 | 处理 |
| --- | --- | --- |
| `面板拒绝了这次续期（HTTP 403）：该节点已被撤销…` | 管理员撤销过这台节点 | 在面板重新接入，或恢复节点登记 |
| `…公钥指纹不一致…` | 本机 `certs/node.key` 与面板登记的不是同一把钥匙（例如从旧包带过来的证书） | 用 `atl-daemon enroll --panel … --token …` 重新走一次凭证交换 |

---

## 5. 关键配置说明

### 5.1 面板（config.yaml）

```yaml
server:
  listen: ":8080"              # HTTP（内网访问）
  tls_listen: ":8443"          # HTTPS（配置证书后生效）
  grpc_listen: ":9090"
  web_dir: "web/dist"
  grpc_mtls: true              # ← 生产必须为 true
  pki_dir: "data/pki"
  trust_proxy: false           # 位于 frp/反代之后时改为 true
  grpc_public_address: ""      # 节点连接面板的地址（一键部署时下发）
db:
  backup_enabled: true         # 面板数据库自动备份
  backup_keep: 7
auth:
  jwt_secret: ""               # 留空 = 自动生成并持久化（推荐）
```

### 5.2 必须修改的项

| 配置 | 说明 |
|------|------|
| `grpc_mtls` | 生产环境设为 `true`，否则任何进程都能伪装成节点 |
| `tls_cert` / `tls_key` | 暴露公网前务必配置（见 `docs/CERTIFICATES.md`） |
| `trust_proxy` | 经 frp / Nginx 访问时设为 `true`，否则审计与限流会把所有用户当成同一 IP |
| `jwt_secret` | 保持留空即自动生成；若手工设置必须 **≥32 字符**，否则面板拒绝启动 |

---

## 6. 内网穿透（可选）

无公网 IP 时，在公网服务器部署 frps：

```bash
# 公网服务器
FRP_VERSION=0.61.1
mkdir -p /opt/frp && cd /opt/frp
# 下载并解压 frp（或从源码构建：cd 模块缓存 && go build ./cmd/frps）
cat > frps.toml <<'EOF'
bindPort = 7000
auth.method = "token"
auth.token = "换成你的随机token"
EOF

cat > /etc/systemd/system/frps.service <<'EOF'
[Unit]
Description=frp server
After=network.target
[Service]
ExecStart=/opt/frp/frps -c /opt/frp/frps.toml
Restart=on-failure
[Install]
WantedBy=multi-user.target
EOF
systemctl enable --now frps
```

面板侧：**穿透管理** → 添加 frps 服务器（填公网 IP、7000、token）→ 为实例分配线路 / 启用面板访问。

---

## 7. 升级

```bash
# 开发机
./scripts/build-release.sh --version 1.1.0

# 面板
scp dist/atlmcpanel-1.1.0-linux-amd64.tar.gz root@<面板IP>:/tmp/
ssh root@<面板IP> 'cd /tmp && tar xzf atlmcpanel-1.1.0-linux-amd64.tar.gz && \
  /tmp/atlmcpanel-1.1.0-linux-amd64/deploy/install.sh panel'

# 节点：在面板「节点管理」点击「重新部署 / 升级」即可（二进制由面板下发，版本自动一致）
```

**数据库结构变更由内置迁移自动完成**（`schema_migrations` 表记录版本），
升级时无需手工改表；迁移失败会回滚并阻止启动，不会留下半成品状态。

> 从 0.11.2 及更早（面板以 root 运行）升级到 0.11.3 时，`deploy/upgrade.sh`
> 会读单元文件里的 `User=` 并把 `data/`、`certs/`、`logs/` 交给该用户、
> `config.yaml` 改成 `root:组 0640`。**漏了这一步的表现是启动即报
> `SQLITE_READONLY`/`permission denied`**，手工补一句
> `chown -R <用户>:<组> /opt/mcpanel/{data,certs,logs}` 即可。

---

## 8. 备份与恢复

### 面板自身
- 启动时自动备份、每 24 小时一次，保留 7 份，位于 `data/backups/`
- 备份接口：`GET/POST /api/panel-backups`
- **恢复方法**：停止面板 → 用备份文件替换 `data/mcpanel.db` → 删除 `-wal`/`-shm` 文件 → 启动

```bash
systemctl stop atlmcpanel-panel
cp /opt/mcpanel/data/backups/panel-YYYYMMDD-HHMMSS.db /opt/mcpanel/data/mcpanel.db
rm -f /opt/mcpanel/data/mcpanel.db-wal /opt/mcpanel/data/mcpanel.db-shm
systemctl start atlmcpanel-panel
```

### 关键文件（需单独备份）
| 路径 | 内容 | 丢失后果 |
|------|------|---------|
| `data/mcpanel.db` | 全部业务数据 | 用户/授权/隧道全丢 |
| `data/pki/ca.key` | 面板 CA 私钥 | **所有节点证书失效，需重新签发** |
| `data/jwt.secret` | 会话签名密钥 | 用户需重新登录 |
| `certs/panel.key` | 面板 HTTPS 私钥 | HTTPS 不可用 |

### 实例数据
由面板的**定时备份**功能负责（每实例可配置间隔与保留份数）。备份落在哪个目录取决于节点上的配置：

- 节点配了**备份根目录**（`backup_root`，建议指向另一块盘/网络盘）→ `<backup_root>/<实例ID>/`
- 没配 → 实例目录下的 `backups/`

> ⚠️ **配了备份根目录之后，备份就不在实例所在盘上了** —— 磁盘告警要相应去看那个挂载点。
> 换备份根目录会让**老链与新链脱钩**：新目录里找不到旧链尾，于是下一轮自动写一份新基线
> （不会把老链搞成"半条"，但要恢复老链得老目录还在）。

#### 自动备份是增量的（0.11.2 起）

- 首份自动备份＝**完整基线**；其后每份只含**自上一环以来变化的文件**（按 `大小 + 修改时间` 判定）
- 恢复时点**任意一环**都会自动还原整条链（基线 → 各层增量 → 删除记录），得到完整目录；
  链上任何一个包损坏都会**拒绝恢复并指出是哪个包**，且**不改动实例目录**
- 保留策略**按链淘汰**：一条链要么整体保留、要么整体删除，一条 N 环的链只占 1 个名额
- **手动备份永远完整**，且不依赖任何链（出事时的最后一道保险）
- 判据精度：`大小 + 修改时间`，修改时间比到**纳秒**（同一秒内、大小不变的改动也能认出来）

---

## 9. 故障排查

| 现象 | 排查 |
|------|------|
| 面板无法启动 | `journalctl -u atlmcpanel-panel -n 50`；常见原因：端口占用、JWT 密钥过短、配置文件语法错误 |
| 节点显示离线 | `journalctl -u atlmcpanel-daemon -n 50`；确认 `panel_address` 可达、证书 `node_id` 与配置一致 |
| 节点连不上（证书错误） | 见 `docs/MTLS.md` 的排查表 |
| 实例启动失败 | 打开实例控制台查看日志；检查 jar 是否存在（核心页面）、内存是否足够 |
| 面板数据库锁 | 已启用 WAL 与 busy_timeout；若仍出现请检查磁盘是否已满 |
| 磁盘告警 | 清理旧备份（注意备份可能在节点配的 `backup_root` 那块盘上，不在实例盘）、节点上的 `logs/` |

### 健康检查

```bash
curl -s http://127.0.0.1:8080/api/health
# {"status":"ok","time":"..."}
```

---

## 10. 生产检查清单

> 2026-11-09 在首台生产机（Debian 13，面板 `:55555`、安全入口、节点与面板同机）上逐条核过，
> 结果标在每条后面。没打勾的两条是**需要运维自己决定或动手**的，不是代码没做。

- [x] `server.grpc_mtls: true`，且所有节点已使用签发的证书接入
      *（`grpc_mtls: true`；`node-001` 走 ② 面板发起接入，证书 `certs/node.crt` 30 天自动续期）*
- [x] 配置了 HTTPS 证书（自签亦可，正式证书更佳）
      *（自签 `certs/panel.crt`，`tls_listen :55556`，外网 200；浏览器会提示"连接不安全"）*
- [x] `auth.jwt_secret` 已由面板自动生成（`data/jwt.secret` 存在且权限 0600）
      *（`-rw------- atlpanel:atlpanel 64B`）*
- [x] 经代理访问时 `trust_proxy: true`
      *（本机直连，不经过代理，保持 `false`；将来挂 frps/反代到面板域名时要打开，
      否则日志里的来源 IP 全是 127.0.0.1）*
- [ ] 已备份 `data/pki/ca.key`（离线保存）
      *文件在（`data/pki/ca.key`，0600）；**离线副本要运维自己拷走** —— 丢了等于所有
      节点证书作废、要重新签发*
- [x] 面板数据库自动备份已启用且验证过可恢复
      *（`enabled: true, keep: 7`；2026-11-09 手动建了一份 `panel-20261109-184424.db`，
      `pragma integrity_check` = ok，里面有 3 个账号 / `mc-main` / `node-001`。
      注意安装时那份 `panel-20261109-164355.db` 是**建管理员之前**拍的，恢复它等于空库）*
- [ ] 各实例已配置定时备份
      *（`mc-main` 目前没有定时备份 —— 需要先定一个**不冲突的开服窗口**再开：
      对运行中的 MC 实例备份会先 `save-off`/`save-all`，玩家会感到一次卡顿）*
- [x] 已确认告警中心无异常告警
      *（`GET /api/alerts` → `active_count: 0`）*
- [x] SSH 凭据（用于一键部署）仅保存在受限访问的面板数据库中
      *（一个都没存：`GET /api/nodes` 里 `ssh_user=''`、`has_auth=false`）*
