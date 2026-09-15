# 架构详细设计

> 补充 `README.md` 与 `DEVELOPMENT.md` 的架构细节。

---

## 1. 分层与组件

### Panel（中心面板）
```
dsh-panel
├── HTTP/WS 层（前端 API + 控制台代理）
│   ├── REST API      (/api/...)
│   └── WebSocket     (/ws/console/{instanceId})
├── gRPC Server（Daemon 接入）
├── 业务层
│   ├── auth / RBAC
│   ├── node mgmt
│   ├── instance mgmt
│   ├── metrics collector
│   ├── tunnel engine
│   └── audit
├── 数据访问（PostgreSQL）
└── 证书签发（mTLS CA）
```

### Daemon（每实例一个）
```
dsh-daemon
├── gRPC Client（连 Panel，mTLS）
├── MC 进程管理（systemd unit）
├── 控制台桥接（stdin/stdout ↔ gRPC stream）
├── 监控探针（CPU/mem/TPS/players）
├── RCON 客户端
└── frpc 管理器
```

---

## 2. 关键交互时序

### 2.1 节点登记（Daemon 接入）
```
管理员 → Panel: 提交节点信息(IP/SSH)
Panel → SSH → VM: 探测环境、部署/启动 dsh-daemon
Panel → 签发 mTLS 客户端证书
Daemon → gRPC Register(cert) → Panel
Panel → 记录节点 online
```

### 2.2 控制台实时流
```
浏览器 ──WS──▶ Panel ──gRPC bidi-stream──▶ Daemon ──stdin/stdout──▶ MCFolia
（xterm.js）      （代理转发）              （systemd 进程）
```

### 2.3 穿透（frp）
```
管理员 → Panel: 给实例分配公网端口 25566
Panel: 生成 frpc 配置（local_port=实例端口, remote_port=25566）
Panel → gRPC ApplyTunnel → Daemon
Daemon → 写 frpc.ini → 启动 frpc → 连 frps
公网玩家 → frps:25566 → frpc → 实例 VM:25565 → MCFolia
```

---

## 3. 控制台技术细节

- 前端：xterm.js 渲染，WebSocket 连接 Panel
- Panel：把一个 WS 连接映射到一个 gRPC bidi-stream（每实例一个 stream，多浏览器共享）
- Daemon：MC 进程 stdout → 按行 → ConsoleFrame(output)；收到的 CommandFrame → 写 MC stdin
- 需处理：彩色 ANSI 转义（xterm 原生支持）、历史日志回放（最近 N 行缓存）

---

## 4. 监控技术细节

- CPU/内存：读 `/proc` 或 cgroup（单位：实例 VM 整体或 MC 进程级，待定）
- TPS / 在线玩家：MCFolia 无原生 RCON 的 Folia 需用 **控制台命令 + 日志解析** 或插件上报（Folia 支持 RCON 但部分插件需适配）
  - 优先：控制台定时发 `tps` / `list` 命令解析输出
- 时序存储：Panel 内存环形缓冲 + 定期落库（或直接用 PostgreSQL，视数据量）

---

## 5. 隔离与安全

- 实例间隔离：PVE VM 层（内核级），面板不做容器隔离
- Daemon 与 Panel：mTLS（每个 Daemon 唯一客户端证书）
- SSH 凭据：Panel 数据库加密存储，仅部署/运维用
- 前端会话：JWT，角色 + 实例授权双重校验

---

## 6. 可扩展性预留

- 多节点：Panel 支持管理多个 Daemon 节点（天然 gRPC 多连接）
- 多 frps：`frps_servers` 表支持多公网入口，未来多线路
- 多 MC 类型：`mc_type` 字段预留 folia/paper/velocity 等