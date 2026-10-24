# 节点凭证交换协议（设计稿）

> 状态：**设计已定，未实现**。定稿日期 2026-10-25。
> 关联：[V1.0-PREVIEW-GOALS.md](V1.0-PREVIEW-GOALS.md)（本协议是 1.0.0-preview 的四项之一）、
> [V2.0.0-KICKOFF.md](V2.0.0-KICKOFF.md)（多面板与反向通道在那里）。

---

## 1. 这一篇解决什么问题

现在的节点接入只有一种方式：**面板用 SSH 密码/私钥去连节点，把 Daemon 推上去**。
它把三件事绑死了：

1. **面板必须持有节点的高权凭据** —— 而且是明文存库（底账 S1）；
2. **面板必须能拨到节点**（推部署要 SSH，后续操作要 9091）；
3. **一台节点只能属于一个面板** —— daemon 配置里只有一个 `panel_address` 和一套证书。

凭证交换要解决第 1 条（凭据不再必需）与第 3 条的**地基**（把「节点身份」从面板的 DB 里
解耦出来，为 V2 的多面板留出结构）。第 2 条属于反向通道，归 V2。

---

## 2. 五种接入方式（UI 下拉框的最终形态）

| # | 名称 | 谁产生 token | 谁发起 | 面板需要节点 SSH 凭据 | 需要面板能拨节点 |
|---|---|---|---|---|---|
| ① | **一键部署远端节点** | — | 面板推 | **要** | 要（SSH + 9091）|
| ② | **凭证交换 · 面板发起** | 面板 | 节点拉 | 不要 | 不要 |
| ③ | **凭证交换 · 节点发起** | 节点 | 节点拉 | 不要 | 不要 |
| ④ | **本机探测连接** | — | 面板连回环 | 不要 | 本机，天然可达 |
| ⑤ | **本机一键部署** | — | 面板本地安装 | 不要 | 本机，天然可达 |

① 是现状（原「SSH」模式，本次改名）。②③ 是同一套 Enroll 协议的两个入口，
差别只在 token 由谁签发、经过谁的键盘。④⑤ 不涉及 token（靠「面板本来就有这台机器的权限」）。

---

## 3. 核心：节点身份与证书

### 3.1 node_uid（新增，本次要落地）

```
node_uid = 节点自己生成的 UUIDv4，首次启动写入 state/node_uid
name     = 显示名（可改，不再是身份）
```

**为什么必须引入**：现在节点身份 = 面板 DB 里的 `name`，证书 CN 也是它。
一旦同一台节点出现在两个面板（V2）或改了显示名，撤销与对账就全乱。
改成：**证书里带 `node_uid`（SAN URI/其他Name），CN 仍写显示名便于排查**。
面板按 node_uid 建唯一索引；显示名冲突只提示、不阻止。

### 3.2 密钥对由节点生成（本次要改的既有实现）

现在的 `pki.IssueClientCert(nodeName)` 是**面板生成私钥再下发**。
走凭证交换时这不可接受：私钥会经过网络，token 泄露 ≈ 身份泄露。

新流程：

```
节点本地生成 keypair（ed25519 或 ec-p256），私钥永不出节点
  → 生成 CSR（CSR 里带 node_uid 与显示名）
  → 带着 token 提交给面板
  → 面板校验 token、用 CA 签 CSR
  → 回证书链（不含私钥）
```

需要新增 `pki.SignCSR(csrPEM, nodeUID, displayName) (certPEM, err)`，
保留旧 `IssueClientCert` 给 ①/④/⑤ 用（那三种面板本来就有机器控制权，私钥下发可接受）。

---

## 4. token 的格式与绑定

```
atl-join-v1.<base64url(payload)>.<base64url(sig)>

payload = {
  v: 1,
  panel_grpc: "1.2.3.4:9090",      // 节点连面板用的地址（含端口，支持面板端口映射）
  panel_fp:   "sha256:ab:cd:...",  // 面板 TLS 指纹，节点钉死，防中间人
  node_name:  "node-007",           // 期望的显示名（可改）
  node_pubkey_hash: "sha256:...",   // ★ 绑定节点公钥（③ 必有；② 签发时留空）
  exp: 1761000000,                  // 过期时间（默认 15 分钟）
  jti: "随机 nonce"                 // 一次性
}
```

### 4.1 两种入口的差异（**这是设计上最要紧的一处**）

| | ② 面板发起 | ③ 节点发起 |
|---|---|---|
| 谁签 sig | **面板**（HMAC，用面板自己的一把 join secret）| **节点**（用节点私钥签名）|
| node_pubkey_hash 何时确定 | **签发时不知道** → 留空，Enroll 时才记录 | **签发时就带上**（节点自己签的）|
| 防「token 被人捡走」 | 靠：15 分钟 + 一次性 + 由管理员亲手粘贴到节点 | 靠：签名自证 + 一次性 |
| 防「伪造接入」 | 面板 HMAC 校验 | 面板验签（说明对方确实持有该私钥）|

> ⚠️ ② 模式**无法在签发时绑定公钥**（面板还不知道节点是谁），这是协议上的硬限制。
> 补偿措施有三条：① 有效期 15 分钟；② 用后即焚；③ **Enroll 成功后把公钥指纹写进
> enrollment 记录，之后所有续期必须用同一把私钥**。也就是说「绑定公钥」在**续期**这一侧
> 是完全生效的，只在首次接入的 15 分钟窗口内依赖传输信道（管理员亲手粘贴）。

### 4.2 token 存哪、怎么失效

```sql
CREATE TABLE join_tokens (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  jti          TEXT NOT NULL UNIQUE,     -- 一次性标识（对应 payload.jti）
  mode         TEXT NOT NULL,            -- 'panel'（②） / 'node'（③）
  node_uid     TEXT NOT NULL DEFAULT '', -- ③ 有；② 为空（Enroll 时才知道）
  node_name    TEXT NOT NULL DEFAULT '',
  created_by   INTEGER NOT NULL,         -- 哪个用户签发的
  created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at   DATETIME NOT NULL,
  used_at      DATETIME,                 -- 非空 = 已用（一次性）
  used_from    TEXT NOT NULL DEFAULT '', -- 用它的来源 IP（审计）
  revoked_at   DATETIME
);
```

---

## 5. Enroll 接口

```
POST /api/enroll            （**不要求登录**，靠 token 鉴权）
Content-Type: application/json
{
  "token":      "atl-join-v1.…",
  "csr":        "-----BEGIN CERTIFICATE REQUEST-----",
  "node_uid":   "uuid",          // ② 模式必填；③ 模式与 token 内一致
  "node_name":  "node-007",
  "daemon_ver": "0.10.0",        // 便于面板识别老节点
  "hostname":   "HK8194921"
}

200 → {
  "cert": "-----BEGIN CERTIFICATE-----",
  "ca":   "-----BEGIN CERTIFICATE-----",
  "panel_grpc": "1.2.3.4:9090",
  "node_uid": "uuid",             // 面板确认后的最终 uid（② 首次生成时由面板兜底）
  "renew_after": "2026-11-05T00:00:00Z"
}
```

**安全要求（逐条都要有测试）**：

| 项 | 要求 |
|---|---|
| 限速 | 每个来源 IP + 全局各有一条令牌桶（Enroll 是未鉴权端点）|
| token 校验顺序 | 存在 → 未撤销 → 未过期 → 未使用（**原子性**：用一条 UPDATE … WHERE used_at IS NULL 抢占）|
| CSR 校验 | 签名有效、公钥类型限定（ed25519/p256）、CSR 内 node_uid 与请求一致 |
| 名称冲突 | 显示名重复 → 自动加后缀，不报错（name 不是身份）|
| 审计 | 成功/失败都记（含来源 IP、jti、node_uid），失败不泄漏具体哪一步错 |
| 关闭 | 面板可一键关闭「凭证交换」入口（等价 410 Gone）|

---

## 6. 撤销与续期

### 6.1 为什么不用 CRL / OCSP

CRL 要分发、要处理缓存与过期；OCSP 要一个长期在线的响应器（多一个必须高可用的组件）。
本项目规模是几十个节点，两者都是纯负担。改用下面这套，效果等价且更简单：

### 6.2 三重机制

| 机制 | 做法 | 生效时间 |
|---|---|---|
| **短 TTL** | 节点证书 **30 天**有效 | 最坏窗口 30 天 |
| **撤销表** | `node_enrollments.revoked_at`，每次 Register / 心跳都查一次 | 下一次心跳（秒级）|
| **主动通知** | 撤销后心跳回执带 `{"revoked": true}`，节点收到立即停止重连并清掉本地证书 | 秒级，且不需要面板能拨节点 |

> 第三行是关键：撤销**不依赖**面板能连到节点（我们本来就不一定能连过去）。
> 节点自己心跳时会看到「你已被移除」，然后自清理 —— 这也是 V2 反向通道的设计前提。

### 6.3 续期

```
证书到期前 7 天，节点用**现有证书**建立 mTLS 连接 → POST /api/enroll/renew
  · 不要求 token（凭据就是那张仍然有效的证书）
  · 面板核对：证书未过期、未撤销、公钥指纹与 enrollment 记录一致
  · 签新证书（同一 node_uid），旧证书记 replaced_by
```

---

## 7. 数据模型（迁移）

```sql
-- 迁移 v31
ALTER TABLE nodes ADD COLUMN node_uid TEXT NOT NULL DEFAULT '';      -- 唯一索引（空值不参与）
ALTER TABLE nodes ADD COLUMN enroll_mode TEXT NOT NULL DEFAULT '';   -- ssh/panel/node/local-probe/local-install
CREATE TABLE node_enrollments (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  node_uid      TEXT NOT NULL,
  cert_fp       TEXT NOT NULL,            -- 证书指纹（sha256），用于精确撤销
  pubkey_fp     TEXT NOT NULL,            -- 公钥指纹：续期必须一致
  panel_ca_id   TEXT NOT NULL DEFAULT '', -- 哪把 CA 签的（为 V2 多 CA 留位）
  issued_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at    DATETIME NOT NULL,
  revoked_at    DATETIME,
  replaced_by   TEXT NOT NULL DEFAULT ''  -- 续期后指向新 cert_fp
);
```

> `enroll_mode` 是给 UI 用的：不同模式能做的操作不同（比如「一键部署远端节点」可以重新
> 推二进制，凭证交换模式只能让对方重新交换）。

---

## 8. 与现有实现的关系

| 现有 | 处理 |
|---|---|
| `pki.IssueClientCert` | **保留**，给 ①④⑤ 用 |
| 新增 `pki.SignCSR` | ②③ 用 |
| `nodes.ssh_auth_type`（v29）| 保留，①模式用 |
| `nodes.panel_grpc_port`（v30）| 保留；V2 多面板时改为 per-enrollment |
| `DELETE /api/nodes/{id}/credential` | 保留（清 SSH 凭据）|
| 现有「无 token 也能注册」的 Register | **收紧**：配置项 `server.require_enrollment: true` 时，未登记的 node_uid 一律拒 |

> ⚠️ 最后一行是**行为变更**：开启后，老节点（没有 node_uid/证书）会连不上，需要走一次
> 迁移（面板发一个「升级 token」，老节点用它跑一次 Enroll 换证书）。这个开关在 preview 默认
> **关闭**，等迁移脚本就绪再打开。

---

## 9. 实施顺序（每一步都可独立发版）

| 步 | 内容 | 依赖 | 风险 |
|---|---|---|---|
| 0 | ①④⑤ 收尾：改名、本机一键部署真安装 | — | 低 |
| 1 | `node_uid` 落地（daemon 生成并持久化；证书带 SAM；面板建索引）| — | 中（证书格式）|
| 2 | `pki.SignCSR` + join_tokens 表 + `POST /api/enroll`（先只做 ③）| 1 | 中 |
| 3 | ③ 的 UI（daemon 打印 token → 面板粘贴）+ 撤销与续期 | 2 | 中 |
| 4 | ② 的 UI（面板签发 token → 节点一条命令）| 3 | 低（同一套 Enroll）|
| 5 | `require_enrollment` 开关 + 老节点迁移 token | 4 | **高**（会切断老节点）|

---

## 10. 待定 / 留给 V2

- **多面板**：同一节点接入多个面板/控制应用。需要 daemon 支持多份 enrollment（配置、证书目录、
  服务端多 CA 校验）+ 节点角色分级。见 V2 文档。
- **反向通道**：面板不再需要拨到节点的 9091，从而让「面板装在 NAT 后」成为可能。
- **token 的签名算法**：② 用 HMAC（面板单方）；③ 用节点私钥签名（ed25519/p256）。
  是否统一改成面板 CA 的离线签名，等 ②③ 都跑起来后再评估。
