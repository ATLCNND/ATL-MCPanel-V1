# Panel ↔ Daemon mTLS 配置指南

ATL-MCPanel 使用**双向 TLS（mTLS）**保护 Panel 与各节点 Daemon 之间的 gRPC 通信：

- 节点必须持有由**面板 CA** 签发的证书才能接入面板（防止伪装成 Daemon）
- 面板调用节点时也会出示自己的客户端证书，节点同样校验（双向认证）

---

## 1. 架构

```
                    ┌──────────────────────────────┐
                    │  Panel（CA 持有者）           │
                    │  gRPC :9090 (mTLS 服务端)     │
                    │  ← 校验 Daemon 客户端证书      │
                    └──────────────────────────────┘
                       ▲ 注册/心跳        │ 指令/查询
                       │                  ▼
                    ┌──────────────────────────────┐
                    │  Daemon（节点）               │
                    │  gRPC :9091 (mTLS 服务端)     │
                    │  ← 校验 Panel 客户端证书       │
                    └──────────────────────────────┘
```

证书构成：

| 证书 | 位置 | CN / SAN | 用途 |
|------|------|----------|------|
| CA | `<pki_dir>/ca.crt` | ATL-MCPanel CA | 签发并校验所有证书 |
| Panel 服务端证书 | `<pki_dir>/panel-server.crt` | SAN: `atlmcpanel-panel` | 供 Daemon 校验面板身份 |
| Panel 客户端证书 | `<pki_dir>/panel-client.crt` | CN: `atlmcpanel-panel` | 面板调用 Daemon 时出示 |
| 节点证书 | 节点上的 `cert_file` | SAN: `<节点名>` | 双向：Daemon 接入 + 作为服务端 |

> **为什么必须用 SAN？** Go 1.15+ 不再接受仅依赖 CN 的证书
> （`x509: certificate relies on legacy Common Name field`）。
> 面板自动生成的证书均已内置 SAN，无需手工处理。

---

## 2. 面板端配置

```yaml
server:
  grpc_listen: ":9090"
  grpc_mtls: true                  # 启用双向认证（生产必须为 true）
  pki_dir: "data/pki"              # CA 与证书存放目录
```

首次启动面板会自动生成 CA 与面板证书，日志会输出：

```
INFO 已生成新的 CA 证书  ca=data/pki/ca.crt
INFO gRPC 服务启动  addr=:9090  mtls=true
```

目录权限（自动设置）：

```
data/pki/          0700
  ca.key           0600   ← 务必妥善保管，泄露等于所有节点身份可被伪造
  panel-server.key 0600
  panel-client.key 0600
```

---

## 3. 节点接入（签发证书）

### 3.1 从面板获取证书材料

**方式 A：界面/API（推荐）**

管理员登录面板后调用：

```bash
# 按节点名签发（节点尚未登记时也可用）
curl -k -X POST https://<面板>:8443/api/nodes/cert \
  -H "Authorization: Bearer <TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"node_name":"node-001"}' > nodecert.json
```

也可对已登记节点调用 `GET /api/nodes/{id}/cert`。

返回内容：

```json
{
  "ca_cert":     "-----BEGIN CERTIFICATE-----...",
  "client_cert": "-----BEGIN CERTIFICATE-----...",
  "client_key":  "-----BEGIN PRIVATE KEY-----...",
  "server_name": "atlmcpanel-panel",
  "expires_days": 1095
}
```

**方式 B：直接取 CA 证书**（用于手工签发）

CA 证书是公开信息，可直接从 `<pki_dir>/ca.crt` 分发。

### 3.2 在节点上安装

```bash
mkdir -p /opt/mcpanel/certs

# 将三份材料写入文件（内容来自上一步的 JSON）
cat > /opt/mcpanel/certs/ca.crt   <<'EOF'
... ca_cert ...
EOF
cat > /opt/mcpanel/certs/node.crt <<'EOF'
... client_cert ...
EOF
cat > /opt/mcpanel/certs/node.key <<'EOF'
... client_key ...
EOF

chmod 600 /opt/mcpanel/certs/node.key
```

### 3.3 节点配置

```yaml
daemon:
  node_id: "node-001"              # 必须与证书 SAN 中的节点名一致
  panel_address: "127.0.0.1:9090"
  grpc_listen: ":9091"
  tls: true                        # 启用 mTLS
  cert_file: "/opt/mcpanel/certs/node.crt"
  key_file:  "/opt/mcpanel/certs/node.key"
  ca_file:   "/opt/mcpanel/certs/ca.crt"
```

重启 Daemon，成功日志：

```
INFO Daemon gRPC 服务启动  addr=:9091  mtls=true
INFO 已启用 mTLS 连接 Panel  ca=...  cert=...
INFO 注册成功  node_id=node-001
```

> **注意**：`node_id` 必须与签发证书时使用的 `node_name` 完全一致。
> Panel 以数据库中的节点名作为 TLS 服务端校验名（SNI），
> 不一致会报 `x509: certificate is valid for X, not Y`。

---

## 4. 故障排查

| 现象 | 原因 | 处理 |
|------|------|------|
| `certificate relies on legacy Common Name field` | 证书缺少 SAN | 用面板重新签发（面板生成的证书已含 SAN） |
| `certificate is valid for node-001, not node-002` | 节点名与证书 SAN 不一致 | 用正确的节点名重新签发 |
| `tls: bad certificate` | 节点证书不是本 CA 签发 | 确认使用的是当前面板的 CA |
| `connection refused` / 一直连不上 | 面板 `grpc_mtls` 与节点 `tls` 设置不一致 | 两端保持同为开启或同为关闭 |
| `x509: certificate has expired` | 证书过期（节点证书默认 3 年） | 重新签发并替换 |

排查命令：

```bash
# 查看证书 SAN 与有效期
openssl x509 -in /opt/mcpanel/certs/node.crt -noout -subject -issuer -dates -ext subjectAltName

# 验证面板侧证书
curl -k https://127.0.0.1:8443/api/pki -H "Authorization: Bearer <TOKEN>"
```

---

## 5. 迁移与轮换

**从明文迁移到 mTLS**：
1. 面板配置 `grpc_mtls: true` 并重启（此时旧节点会断开）
2. 为每个节点签发证书并按上文安装
3. 节点逐个重启，日志出现「已启用 mTLS 连接 Panel」即完成

**证书轮换**：重新调用签发接口获取新证书，替换节点文件后重启 Daemon。
旧证书在过期前仍然有效，因此可逐个节点平滑轮换。

**CA 轮换**：需删除 `<pki_dir>` 重新生成，并为**所有**节点重新签发（会中断连接）。
生产环境建议在维护窗口进行。

---

## 6. 安全提示

- `data/pki/ca.key` 等价于「签发任意节点身份的权限」，必须限制访问（0600）并纳入备份
- 节点私钥 `node.key` 仅存放在对应节点上，不要跨节点复用
- 面板 API `/api/pki` 与 `/api/nodes/*/cert` 仅管理员可访问
- 与面板 HTTPS 证书（见 `docs/CERTIFICATES.md`）是两套独立体系：
  前者保护 **面板↔浏览器**，后者保护 **面板↔节点**
