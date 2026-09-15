# ATL-MCPanel 开发文档

> 本文档是项目的**唯一权威开发上下文**，供所有开发 agent（含跨会话/跨机器搬迁）阅读。
> 阅读顺序：本文件 → `ARCHITECTURE.md` → `ENVIRONMENT.md` → `ROADMAP.md`。

---

## 目录

1. [项目概述](#1-项目概述)
2. [代码仓库与协作规范](#2-代码仓库与协作规范)
3. [环境与机器清单](#3-环境与机器清单)
4. [本地开发工作流](#4-本地开发工作流)
5. [目录结构规范](#5-目录结构规范)
6. [技术栈与依赖](#6-技术栈与依赖)
7. [数据模型](#7-数据模型)
8. [gRPC 协议定义](#8-grpc-协议定义)
9. [RBAC 权限模型](#9-rbac-权限模型)
10. [模块设计](#10-模块设计)
11. [阶段里程碑 (Roadmap)](#11-阶段里程碑-roadmap)
12. [安全与密钥管理](#12-安全与密钥管理)
13. [已定稿决策清单（勿重复讨论）](#13-已定稿决策清单)

---

## 1. 项目概述

**ATL-MCPanel** 是一个 MCFolia（Paper/Folia 系）服务端的多实例管理系统，对标 MCSM / SimpFun。

- **Panel**：中心管理面板（多用户、RBAC、可视化控制台、数据库、frp 穿透分配）
- **Daemon**：部署在每台 MC 实例 VM 内的守护进程（管理 MC 进程、上报状态、执行穿透）

完整定位与架构图见 `README.md`。

---

## 2. 代码仓库与协作规范

- **仓库地址**（HTTPS）：`https://github.com/ATLCNND/ATL-MCPanel.git`
- **仓库可见性**：Private（私有）
- **默认分支**：`main`
- **GitHub 账号**：`ATLCNND`

### 2.1 认证凭据（⚠️ 见 ENVIRONMENT.md 存放，绝不放代码库）

- GitHub PAT（classic，全 repo 权限）：见 `ENVIRONMENT.md`
- 各服务器 SSH 凭据：见 `ENVIRONMENT.md`

### 2.2 提交规范

- 分支策略：`main` 为主干，功能开发用 feature 分支，合回 main 用 PR（单人开发可简化直接推 main）
- Commit message 风格：`type(scope): 描述`，如 `feat(daemon): 实现实例启停`
- 每个阶段 (M1~M6) 完成打 tag：`m1`、`m2` ... 

### 2.3 源码双份存放

> 用户要求：**源代码放在当前工作区（Windows `F:\server`），仓库 `github.com/ATLCNND/ATL-MCPanel` 也留一份。**

即：本地开发在 Windows 工作区，定期 push 到 GitHub 同步。Debian 虚拟机作为**编译/运行验证环境**，从 git clone 拉取代码。

---

## 3. 环境与机器清单

> 完整凭据、IP、端口见 `ENVIRONMENT.md`（敏感信息单独隔离）。

| 机器 | 角色 | 说明 |
|------|------|------|
| Windows 开发机 | 写代码 | 工作区根目录 `F:\server`，项目在 `F:\server\mcpanel` |
| Debian VM（地址按你自己的环境填） | 开发验证环境 | 装 git/go，编译运行 Panel+Daemon，跑测试 folia 实例 |
| 公网云服务器 | frps | M5 阶段才需要，高带宽 |
| PVE 宿主机 | 生产环境 | 最终部署目标，D3 大内存 |

---

## 4. 本地开发工作流

### 4.1 前端（已集成到 Panel 托管，推荐）

Panel 内置静态资源托管，**日常使用无需单独启动前端服务**：

```bash
# 1) 构建前端
cd web && npm run build          # 产物在 web/dist

# 2) 浏览器访问 http://<panel-host>:8080
#    Panel 按 config.yaml 的 server.web_dir 托管（默认 web/dist）
```

特性：
- SPA 回退：未命中的前端路由返回 `index.html`
- `/assets/**`（Vite 带 hash 产物）→ 长缓存 `immutable`；`index.html` → `no-cache`
- `/api/**`、`/ws/**` 未知路径返回 404 JSON，不做 SPA 回退
- 含目录穿越防护（详见 `internal/panel/httpapi/static.go`）

更新前端只需重新 `npm run build` 并替换 `web/dist`，**不必重启 Panel**，浏览器刷新即可。

### 4.2 前端热更新开发（改前端代码时）

```bash
cd web && npm run dev            # vite dev server（默认 5173）
```

vite 会把 `/api`、`/ws` 代理到 Panel（见 `web/vite.config.ts` 的 target）。
**注意**：vite dev 只监听 IPv6 `::1`，请用 `http://localhost:5173` 访问
（用 `127.0.0.1:5173` 会 ECONNREFUSED）。

### 4.3 后端开发

- Windows 工作区编辑代码（**本机无 Go**），在 Debian 上编译运行
- 同步方式：`git push` → Debian `git pull`；GitHub 不通时用 SFTP 直传
- 重新生成 protobuf：`scripts/gen-proto.sh`（需 protoc + 插件，已装在 Debian）

### Windows 开发机
- 工作目录：`F:\server\mcpanel`
- 工具：Node.js v22、Git 2.55（**无 Go**）

### Debian 验证机
- 已装：git 2.47.3、go 1.24.4、gcc/g++、protoc 3.21.12 + Go 插件、sqlite3、JDK 21
- 从 GitHub clone 或 SFTP 同步代码，编译、运行、验证
- 通过 SSH 连接（凭据见 ENVIRONMENT.md）

---

## 5. 目录结构规范

```
mcpanel/
├── README.md               # 项目定位与架构图
├── docs/
│   ├── DEVELOPMENT.md       # 本文：开发上下文（权威）
│   ├── ARCHITECTURE.md      # 详细架构设计
│   ├── ENVIRONMENT.md       # 凭据/机器/工具状态（敏感，勿提交任何真实密码到 git 之外）
│   ├── ROADMAP.md           # 里程碑与任务拆解
│   └── API.md               # gRPC/HTTP 接口文档
├── proto/                   # protobuf 定义
│   └── mcpanel.proto        # 核心协议（Panel↔Daemon）
├── cmd/
│   ├── panel/               # dsh-panel 入口
│   └── daemon/              # dsh-daemon 入口
├── internal/
│   ├── panel/               # Panel 业务逻辑
│   │   ├── server/          # gRPC server
│   │   ├── http/            # HTTP API（前端）
│   │   ├── auth/            # 认证与 RBAC
│   │   ├── db/              # 数据访问层（PostgreSQL）
│   │   └── tunnel/          # frp 分配引擎
│   ├── daemon/              # Daemon 业务逻辑
│   │   ├── mcprocess/       # MC 进程管理（systemd）
│   │   ├── console/         # 控制台流
│   │   ├── monitor/         # 监控探针
│   │   ├── rcon/            # RCON 客户端
│   │   └── frpc/            # frpc 管理
│   ├── proto/               # 生成的 protobuf 代码
│   └── common/              # 公共库（配置、日志、证书）
├── web/                     # 前端（React + Vite + TS）
│   ├── src/
│   ├── package.json
│   └── vite.config.ts
├── scripts/                 # 部署/构建脚本
│   ├── install-daemon.sh    # Daemon 安装脚本
│   ├── build.sh
│   └── gen-proto.sh         # 生成 protobuf 代码
├── go.mod
└── go.sum
```

---

## 6. 技术栈与依赖

### 后端（Go）
| 用途 | 选型 | 说明 |
|------|------|------|
| 语言 | Go 1.24+ | Debian 已装 1.24.4 |
| gRPC | google.golang.org/grpc | Panel↔Daemon 通信 |
| protobuf | google.golang.org/protobuf | 协议定义 |
| HTTP | gin 或 net/http + chi | 前端 API（待定，倾向 gin）|
| WebSocket | gorilla/websocket 或 nhooyr.io/websocket | 前端控制台流 |
| 数据库 | PostgreSQL (`lib/pq` 或 `pgx`) | 生产；SQLite 可作开发起步 |
| SSH 客户端 | golang.org/x/crypto/ssh | Panel 部署 Daemon |
| 证书 | crypto/x509 | mTLS 签发 |
| 配置 | viper 或 yaml.v3 | 配置文件解析 |

### 前端（React）
| 用途 | 选型 |
|------|------|
| 框架 | React 18 + Vite + TypeScript |
| 终端组件 | xterm.js + @xterm/addon-fit |
| 状态管理 | zustand 或 react-query |
| UI | 待定（Ant Design / Mantine / 自制） |
| HTTP/WS | axios + 原生 WebSocket |

### 验证环境依赖（Debian 需额外装）
- 运行 folia 测试需要 **JDK 21+**（M2 阶段安装）

---

## 7. 数据模型

PostgreSQL 表设计（初稿）：

```sql
users 用户
  id            bigserial PK
  username      text UNIQUE
  password_hash text          -- bcrypt
  role          text          -- admin/operator/user
  status        text          -- active/disabled
  created_at    timestamptz

roles / permissions  RBAC
  role           text
  permission     text          -- 细粒度权限点

nodes 节点（实例 VM）
  id            bigserial PK
  name          text
  ip            text
  ssh_user      text
  ssh_auth      text          -- 加密存储（密码/密钥）
  ssh_port      int
  status        text          -- online/offline
  cpu           int
  mem           bigint
  last_seen     timestamptz

instances MCFolia 实例
  id            bigserial PK
  node_id       bigint FK(nodes)
  name          text
  mc_type       text          -- folia/paper
  java_version  text          -- 21 等
  port          int
  max_mem       text          -- 如 2G
  status        text          -- running/stopped/starting
  image_template text         -- PVE 模板名（记录用）
  created_at    timestamptz

instance_assignments 实例授权
  id            bigserial PK
  instance_id   bigint FK
  user_id       bigint FK
  level         text          -- owner/collab/viewer
  UNIQUE(instance_id, user_id)

tunnels 穿透映射
  id            bigserial PK
  instance_id   bigint FK
  protocol      text          -- tcp
  local_port    int
  public_port   int
  frps_id       bigint
  status        text

frps_servers frp 服务端
  id            bigserial PK
  name          text
  host          text
  port          int
  token         text          -- 加密

audit_logs 审计日志
  id            bigserial PK
  user_id       bigint FK
  action        text
  target        text
  detail        jsonb
  ip            text
  created_at    timestamptz
```

---

## 8. gRPC 协议定义

伪接口（实际在 `proto/mcpanel.proto` 中用 protobuf 定义，字段名以此为准）：

```proto
service DaemonService {
  // 节点注册（首次握手）
  rpc Register(RegisterRequest) returns (RegisterResponse);

  // 实例操作
  rpc CreateInstance(CreateInstanceRequest) returns (CreateInstanceResponse);
  rpc StartInstance(InstanceRequest) returns (OperationResponse);
  rpc StopInstance(InstanceRequest) returns (OperationResponse);
  rpc RestartInstance(InstanceRequest) returns (OperationResponse);
  rpc DeleteInstance(InstanceRequest) returns (OperationResponse);

  // 控制台（bidi stream：双向流）
  rpc Console(stream ConsoleFrame) returns (stream ConsoleFrame);

  // 监控数据流（server stream：Daemon → Panel）
  rpc StreamMetrics(InstanceRequest) returns (stream Metrics);

  // 配置读写
  rpc GetConfig(ConfigRequest) returns (ConfigResponse);
  rpc SetConfig(ConfigRequest) returns (ConfigResponse);

  // frp 隧道管理
  rpc ApplyTunnel(TunnelRequest) returns (OperationResponse);
  rpc RemoveTunnel(TunnelRequest) returns (OperationResponse);

  // 备份
  rpc Backup(BackupRequest) returns (BackupResponse);
  rpc Restore(RestoreRequest) returns (OperationResponse);
}
```

关键消息（框架）：

```proto
message ConsoleFrame {
  oneof payload {
    CommandRequest command = 1;   // Panel → Daemon，发送命令
    ConsoleOutput output = 2;      // Daemon → Panel，日志/输出
    ControlSignal signal = 3;      // 打开/关闭流、附加/分离
  }
}

message Metrics {
  double cpu_percent = 1;
  uint64 mem_used = 2;
  uint64 mem_total = 3;
  double tps = 4;
  int32 players_online = 5;
  int32 players_max = 6;
  int64 timestamp = 7;
}
```

---

## 9. RBAC 权限模型

**两层叠加**：全局角色 + 实例级授权关系。

| 角色 | 全局能力 |
|------|---------|
| **superadmin** | 一切：用户管理、角色分配、节点登记、所有实例、穿透分配、镜像管理 |
| **admin** | 节点登记、实例管理、穿透分配、审计查看（不可管其他 admin）|
| **user** | 无可操作全局能力，能力来自实例授权关系 |

实例级授权 level：
- `owner`：启停/控制台/配置/备份/删除
- `collab`：启停/控制台
- `viewer`：只看状态/日志

权限判定：`全局角色权限 ∪ 实例授权权限`。

---

## 10. 模块设计

### Panel 模块
1. **auth**：登录、JWT/session、密码 bcrypt、RBAC 中间件
2. **node mgmt**：节点登记（SSH 探测）、在线状态
3. **instance mgmt**：实例 CRUD，通过 gRPC 调 Daemon
4. **console proxy**：前端 WS ↔ Daemon gRPC stream 的双向代理
5. **metrics collector**：接收 Daemon 上报，写时序，推送前端
6. **tunnel engine**：分配公网端口，生成 frpc 配置，下发 Daemon
7. **audit**：操作审计日志

### Daemon 模块
1. **mcprocess**：用 systemd unit 托管 MC 进程，启停/重启
2. **console**：将 MC stdin/stdout 桥接成 gRPC 流
3. **monitor**：采集 CPU/内存/TPS（RCON 或日志解析）/在线玩家
4. **rcon**：RCON 客户端（发白名单、op 等命令）
5. **frpc**：接收 Panel 下发的隧道配置，启动/管理 frpc 进程

---

## 11. 阶段里程碑 (Roadmap)

详见 `ROADMAP.md`，摘要：

| 阶段 | 目标 | 验证产物 |
|------|------|---------|
| **M1** | Go 工程 + proto + 双二进制 + DB + 登录 | 面板能登录，Daemon 能注册 |
| **M2** | Daemon 拉起 folia + 控制台流 | 页面起服/看日志/发命令 |
| **M3** | RBAC + 实例授权 + 审计 | 角色能力隔离 |
| **M4** | 配置/备份/监控图表 | 完整管理闭环 |
| **M5** | frp 穿透引擎 | 公网连实例端口 |
| **M6** | mTLS/部署/压测/镜像克隆 | 迁移 PVE 实测 |

---

## 12. 安全与密钥管理

- **任何真实密码、token、私钥、公网 IP 凭据一律不进入 git 仓库**（`.gitignore` 排除 `ENVIRONMENT.md` 的敏感部分，或 ENVIRONMENT.md 内只存占位符）
- 密钥通过**环境变量**注入（JWT secret、DB 密码、frps token）
- mTLS 证书由 Panel 动态签发，不预置进仓库
- 生产密码用 bcrypt / 加密存储（DB 中不存明文）

---

## 13. 已定稿决策清单

> 以下决策已经与用户反复确认，**新 agent 不得重新提出替代方案或再次询问用户**，直接按此执行。

1. 架构：**Panel + Daemon 拆分**
2. 隔离：**PVE 虚拟机/LXC 原生隔离**，路线 B（管理员 PVE 手动建 VM → 面板登记，面板不直接调 PVE API）
3. 生产用 **D3 大内存**，实例跑**完整 VM**（不用 LXC）
4. 技术栈：**Go 后端 + React 前端**（开发速度与性能优先）
5. 通信：**gRPC（protobuf）+ mTLS**（由开发方决定，已定为 gRPC）
6. 穿透：**frp**，公网云服务器跑 frps，管理员按实例分配端口，单线路
7. 预制镜像：管理员维护 **PVE 模板**（java 版本），克隆即新实例
8. Daemon 接入：管理员把 VM 的 **SSH 信息登记给 Panel**
9. 源码：Windows 工作区 `F:\server` 为主，GitHub `ATLCNND/ATL-MCPanel` 备份，Debian 编译验证
10. 多用户登录 + 管理员权限分配（RBAC）