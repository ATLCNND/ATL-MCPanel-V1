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
2. 安装二进制与前端静态资源
3. 生成初始 `config.yaml`（**已存在则保留，便于升级**）
4. 生成自签 HTTPS 证书（若安装了 openssl）
5. 安装并启动 `atlmcpanel-panel.service`

> 脚本是**幂等**的：升级时重新解压并再次执行即可，配置与数据不受影响。

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
  grpc_listen: ":9091"
  instance_dir: "instances"
  tls: true
  cert_file: "/opt/mcpanel/certs/node.crt"
  key_file: "/opt/mcpanel/certs/node.key"
  ca_file: "/opt/mcpanel/certs/ca.crt"
```

4. 重启：`systemctl restart atlmcpanel-daemon`

日志出现 `注册成功` 即接入完成。

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
由面板的**定时备份**功能负责（每实例可配置间隔与保留份数），
备份文件位于节点上的 `<实例目录>/backups/`。

---

## 9. 故障排查

| 现象 | 排查 |
|------|------|
| 面板无法启动 | `journalctl -u atlmcpanel-panel -n 50`；常见原因：端口占用、JWT 密钥过短、配置文件语法错误 |
| 节点显示离线 | `journalctl -u atlmcpanel-daemon -n 50`；确认 `panel_address` 可达、证书 `node_id` 与配置一致 |
| 节点连不上（证书错误） | 见 `docs/MTLS.md` 的排查表 |
| 实例启动失败 | 打开实例控制台查看日志；检查 jar 是否存在（核心页面）、内存是否足够 |
| 面板数据库锁 | 已启用 WAL 与 busy_timeout；若仍出现请检查磁盘是否已满 |
| 磁盘告警 | 清理旧备份或节点上的 `<实例目录>/backups/`、`logs/` |

### 健康检查

```bash
curl -s http://127.0.0.1:8080/api/health
# {"status":"ok","time":"..."}
```

---

## 10. 生产检查清单

- [ ] `server.grpc_mtls: true`，且所有节点已使用签发的证书接入
- [ ] 配置了 HTTPS 证书（自签亦可，正式证书更佳）
- [ ] `auth.jwt_secret` 已由面板自动生成（`data/jwt.secret` 存在且权限 0600）
- [ ] 经代理访问时 `trust_proxy: true`
- [ ] 已备份 `data/pki/ca.key`（离线保存）
- [ ] 面板数据库自动备份已启用且验证过可恢复
- [ ] 各实例已配置定时备份
- [ ] 已确认告警中心无异常告警
- [ ] SSH 凭据（用于一键部署）仅保存在受限访问的面板数据库中
