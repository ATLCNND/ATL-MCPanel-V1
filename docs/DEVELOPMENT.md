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
14. [开发约定与技术接缝（2026-09-15 讨论结论）](#14-开发约定与技术接缝2026-09-15-讨论结论)

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

> ⚠️ **第 2、3、7 条需要按第十四节修订**：那几条写于"目标是 PVE"的阶段，
> 说的是"隔离由 PVE VM 提供、管理员手工建 VM、用 PVE 模板克隆"。
> 现在的现实是：**没有 PVE 环境**（本地无法嵌套虚拟化），要纳管的机器包括
> 本地 VM、内网机器、公网 VPS、**别人面板上的实例/容器**。
> 所以"隔离从哪来"变成**运行时可选项**（见第十四节），面板**不绑定 PVE**。
> 历史条款保留不改，是为了不抹掉决策过程。

---

## 14. 开发约定与技术接缝（2026-09-15 讨论结论）

> 这一节是"**写代码时该遵守什么**"与"**为将来留哪些接缝**"的结论，
> 已与项目所有者讨论并认可。新 agent 请直接照此执行，不必重新提案。
> （产品定位/授权/定价等**商业**内容不写在这里，见 `V2.0.0-KICKOFF.md` 等内部文档。）

### 14.1 产物平台：多架构 + 静态编译

> ⚠️ **本节 2026-09-16 按实测修订过**。原写法是"daemon 用 `CGO_ENABLED=0` 静态编译、
> 用 `file` 断言 `statically linked`"，实测**两条都不成立**，详见下面的"踩过的坑"。

| 产物 | 目标 | 做法 |
|---|---|---|
| `dsh-panel`、`dsh-daemon` | `linux/amd64`、`linux/arm64` | **CGO 保持开启 + musl 完全静态链接**（`scripts/build.sh` 已封装） |

**为什么必须静态**：动态链接的二进制继承构建机的 glibc（Debian 13 → 需要 `GLIBC_2.34`），
在旧发行版（CentOS 7 = 2.17）上**直接起不来**，报 `version 'GLIBC_2.34' not found`。
**为什么必须 arm64**：Apple M4 这类机器真实存在于要纳管的范围里。

做法（不改一行代码）：

```bash
CC=musl-gcc CGO_ENABLED=1 go build -tags netgo,osusergo \
  -ldflags '-linkmode external -extldflags "-static"' ...
```

`netgo,osusergo` 不能省：静态链接下走 cgo 的 NSS 查询（用户/域名解析）会失败。
构建机需要 `musl-tools`（amd64）+ `gcc-aarch64-linux-gnu`（arm64）。

#### ⚠️ 踩过的坑：`CGO_ENABLED=0` 会产出"看着通过、其实全坏"的产物

面板用 `mattn/go-sqlite3`（CGO 绑定）。关掉 CGO 后：

| | 结果 |
|---|---|
| `go build` | **成功** |
| `file` 断言 | **`statically linked` —— 通过** |
| 实际启动 | `FATAL 初始化数据库失败: Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work. This is a stub` |

也就是说**产物完全不可用，而构建流水线一路绿灯**，直到用户机器上才炸。
所以验收标准不能只有 `file`，必须**真的启动一次**（`scripts/smoke-binary.sh`：
起进程 → 建库 → 跑迁移 → HTTP 200）。这条教训值得推广到所有"构建产物"类断言上。

> 另一条同样重要的断言：**产物里不应出现任何 `GLIBC_2.x` 符号版本引用**
> （`strings binary | grep -o 'GLIBC_2\.[0-9]*'` 应为空）。
> "statically linked" 是文件格式描述，"没有 GLIBC_ 引用"才是"能跑在旧系统上"的直接证据。

#### 为什么没有改用纯 Go 的 SQLite 驱动

`modernc.org/sqlite` 能彻底去掉 CGO，是个正当方案，但实测代价偏高：
latest 版本要求 **Go 1.26**，会连带下载整套 `golang.org/toolchain`（几百 MB，
在这台机器上十几分钟没下完）；且要改 DSN 语法（`_journal_mode=WAL` →
`_pragma=journal_mode(WAL)`）与驱动名。**为了"静态"去动数据层不划算** ——
musl 方案零代码改动就拿到了同样的静态产物。

### 14.2 无 systemd 的运行方式（容器场景）

要在**容器/全量镜像**（如简幻欢 AIO 那类多运行时镜像）里当节点用，Daemon 必须支持
"**前台直接跑**、被启动脚本 `exec`"，而不是只依赖 systemd 单元：

```bash
exec /opt/mcpanel/bin/dsh-daemon -config /opt/mcpanel/config.yaml
```

日志与退出语义要写清楚（退出码、信号处理、`KillMode=process` 在 systemd 场景下的必要性）。
容器里通常**不能写父 cgroup** ⇒ 能力探测要如实上报"配额不可用"，界面对应置灰。

### 14.3 本机节点自动发现（**V1 就要做**）

单机部署包应当"**装完即用**"：面板启动时探测 `127.0.0.1:<daemon_grpc_port>` 上是否有
一个**尚未登记**的本机 Daemon，若在则自动登记（并在 UI 上给一个「发现本机节点」按钮）。

- 证书必须包含 **`127.0.0.1` 的 IP SAN**（否则本机回连的 mTLS 校验会失败）
- 这条**不依赖**任何新协议，是 V1 就能落地的开箱体验改进

### 14.4 多后端节点的接缝：`NodeClient` 接口

面板现在有 **45 处**直接调用 `pb.DaemonServiceClient` 的方法。要把"其他来源的节点"
（复用他人面板 API、纯监控、Relay 代理…）纳进来，必须先把它们收敛到一个接口：

```go
// 语义：面板对"一个节点"能做的事。不同实现按能力实现子集，不支持的方法返回明确错误。
type NodeClient interface { /* 启停、控制台、文件、备份、监控… */ }
```

两条纪律：
1. **"怎么连"与"连上怎么管"是两个维度**，别混在一个 `kind` 里：
   连接方式（直连 gRPC / 反向通道 / Relay）× 管理后端（自家 Daemon / 第三方面板 API / RCON / 只读监控）。
2. **能力用数据表示**（`caps` 表/结构），不要散落成 `if nodeType == ...`；
   界面按 caps 置灰并给出"为什么点不了"的一句话说明。

> 这也是重构的动机：**先抽接口（纯搬运、不改行为），再加实现**。V1 收尾阶段就抽。

### 14.5 运行时后端抽象（隔离从哪来）

```go
type Runtime interface { /* 创建/启动/停止/销毁一个实例的运行环境 */ }
// native（默认，现状：直接进程 + cgroup v2 + 独立目录）
// nspawn（systemd-nspawn，systemd 原生、开销小、容器里能跑我们的 Daemon）
// docker/podman（生态最好，但要改备份/文件/网络）
```

- **排序理由**：`native` → `nspawn` → `docker`。nspawn 与现有模型最贴合（cgroup/日志/单元几乎不改），
  Docker 能力最强但改动面最大。
- **不在 V1 实现容器化**，只留接口；是否上容器取决于部署形态：
  "每客户一台 VM" 收益低（VM 已给隔离），"一台机器跑多个实例" 收益高（隔离 + 环境一致性）。

> **2026-09-18 更新（用户拍板走 Docker，接缝细节见 `docs/CONTAINERIZATION.md`）**：
> 内测期间发生 T0（实例以 root 运行），修复后仍是 native 模式。用户决定推进容器化，
> 选型定为 **Docker**。已实测节点约束：`overlay` 模块可加载（overlay2 可用）、
> Docker 官方仓库与阿里云镜像均可达（9 MB/s）、**user namespace 被内核禁用**
> （所以没有 rootless，必须 `--user <uid>:<gid>`，否则容器里的 root 就是宿主 root）、
> 只有 cgroup v1（与我们现有的 v1 支持同构）。
> 关键设计：**把 `docker run` 仍当子进程启动**，这样 stdin 管道、stdout 重定向到
> `logs/console.log`、日志尾随这些现有逻辑都不用改；改动集中在"强杀"
> （必须 `docker rm -f`，直接 kill CLI 会留下容器）与限额归属（同一 cgroup 层级
> 只能有一个归属，容器化的实例让 Docker 管，Daemon 跳过）。

### 14.5.1 前端：借鉴 `ElementsPlus-Admin-Template` 的边界（2026-09-18 调研）

用户提出研究 [NingZeStudio/ElementsPlus-Admin-Template](https://github.com/NingZeStudio/ElementsPlus-Admin-Template)
的可利用性。**结论：可借鉴交互与信息架构，不能复用组件。** 依据（取自其 README）：

- 它是 **Vue 3 + TypeScript + Element Plus**，定位是"多系统集成与微前端嵌入"、
  **iframe-first 架构**与 Zinc 低饱和度风格；我们的前端是 **React 18 + Vite + 手写 CSS token**。
  跨框架直接复用组件不成立。
- **真正值得借鉴的是它的 iframe 保活容器与宿主↔子系统通信协议**（IframeBridge）：
  - 常驻 DOM 实例池：激活过的 iframe 用 `v-show` 隐显、DOM 常驻，切标签不丢表单与滚动位置，
    只在关闭标签时才从 DOM 移除；
  - 双向通信：子系统可调用宿主的全局消息组件、改父级标签标题、请求路由跳转、
    打开/关闭标签；宿主切主题时向所有 iframe 广播；
  - 子系统可 `GET_CONTEXT` 取宿主认证令牌与环境参数；
  - **Origin 白名单**过滤跨站消息。
- **对我们的直接价值**：面板里已经有"嵌入外部 Web 工具"的真实需求（BlueMap / Dynmap /
  插件自带后台 / 其它工具），现在没有统一做法。可以照这套设计做一个
  「实例内嵌页面」能力：**标签保活**（切走不重载地图）、**主题跟随**、
  **令牌按需下发且限定 Origin**。这与"面板内跳转"是两件事，值得单列一个功能项。
- 不建议：换栈（React→Vue）或引入 Element Plus（React 侧对应的是 Ant Design/Arco），
  代价是一次全站前端重写，与公开 V1 的 i18n 计划互相冲突。

### 14.6 跨平台边界（明确的"不做"）

- **节点只支持 Linux**：依赖 cgroup v2、`/proc`、进程组信号、`syscall.Kill(-pid)`。
- macOS/Windows **不做节点程序**。替代路径：
  - Windows：文档给"用 WSL2 或 Docker 跑节点"的方案；
  - macOS / 无法装 Daemon 的机器：**HTTP 接入**（对方面板 API）或**跨平台 Relay**（纯网络组件，
    可编 `darwin/arm64`，不碰 `/proc`、cgroup、systemd）。

### 14.7 代码规范（写进 lint 与评审）

| 项 | 约定 |
|---|---|
| **错误返回** | HTTP 用 `writeErr` 中文提示 + 正确状态码（400 参数/403 权限/404 不存在/409 冲突/5xx 内部）；**不要把内部细节回显给前端** |
| **日志** | `slog`：**字段名英文、消息中文**；敏感值（token/密码/凭据）**永不入日志** |
| **注释** | 只写"**为什么**"与"**坑**"，不写"做了什么"；踩过的坑要写清现象+根因+修法 |
| **DB 迁移** | `migrations` 列表**按 Version 排序后执行**（列表顺序 ≠ 版本顺序）；新迁移一律追加版本号 |
| **proto** | 新增字段**先读完整个 message** 再选编号（真实踩过：`InstanceRuntime` 的 `net_scope=16` 排在 `error=11` 之后，撞号导致 protoc 失败） |
| **前端** | 颜色/圆角/间距一律走 CSS 变量；共享组件（如 `Avatar`）不复制；dev 代理地址走环境变量，不写死 |

### 14.8 公开版的体验优先项

- **i18n（中/英）是公开版的第一优先**：没有英文，等于放弃社区反馈。
- 一键开服体验（核心在线下载 + 版本选择 + 建实例向导）优先于更多边缘功能。
- README 要写清**已知限制**（Linux-only 节点、cgroup v2 依赖、软配额、接管态只读、上传上限），
  避免用户踩坑后才发现。

### 14.9 贡献与许可口径（2026-09-15 定案，勿再写成"开源"）

- **许可：PolyForm Noncommercial License 1.0.0**（非商业许可，源码公开）。
  个人学习/自用/教育/公益免费；**对外收费提供服务、随硬件打包售卖、作为商业产品的一部分**
  都不在许可范围内，需另行授权。
  - ⚠️ **用词纪律**：这是 **source-available（源码公开）**，**不是 OSI 认可的开源许可**。
    文档、README、对外沟通一律写"源码公开 / 非商业许可"，**不要写"开源"** ——
    写错了会同时得罪社区（承诺了没给的权利）和买家（语义与 LICENSE 不符）。
  - 所有者的**自家商用不受限**：版权人不受自己发出的许可约束，可双授权（免费非商业 + 付费商业）。
- **贡献：只接受 Issue，不接受外部 PR**（`CONTRIBUTING.md` 已写明理由）。
  目的是保持**版权归属单一** —— 一旦合并外部代码，之后调整授权方式或跨产品线复用
  都要逐个找贡献者同意。因此**不需要 CLA 流程**（没有外部著作权加入）。
- `LICENSE` 正文**必须与官方纯文本逐字一致**（改用 `F:\server\verify_license.js` 校对），
  许可错一个词就可能改变法律含义；中文说明只放 README/CONTRIBUTING，且必须写明
  "以英文原文为准、不构成额外授权"。
- **提交前必须过泄漏门禁**：仓库里不得出现内网地址、真实域名、密码/token、私有环境清单
  （导出公开版时由 `export_public_v1.js` 强制检查，命中即失败）。

### 14.10 发布物的许可证合规（待办，**卖包前必须做**）

我们的依赖里有 **Apache-2.0** 组件（如 `google.golang.org/grpc`），
Apache-2.0 要求**再分发时附带许可副本与 NOTICE**。V1 的部署包里直接塞了二进制，
所以：

- 计划随发布包放一份 `THIRD-PARTY-NOTICES.md`（从 `go.mod` / `go.sum` / `node_modules`
  生成依赖名 + 版本 + 许可类型 + 许可全文链接）。
- 同理，**我们自己在 `LICENSE` 里声明的 `Required Notice:` 行**也要保证随任何副本一起传递
  （PolyForm 的 `Notices` 条款），所以 `Required Notice` 同时写进 `LICENSE` 与 README。