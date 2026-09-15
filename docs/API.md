# API 接口文档（规划）

> gRPC 接口定义在 `proto/mcpanel.proto`，本文记录 HTTP/WS 前端接口语义。
> M1 阶段先定骨架，随实现补充。

---

## 认证约定

- 前端登录后获得 JWT，后续请求带 `Authorization: Bearer <token>`
- WebSocket 连接通过 query 参数带 token 或首帧鉴权（待定）

---

## REST API（规划）

| 方法 | 路径 | 说明 | 阶段 |
|------|------|------|------|
| POST | /api/auth/register | 注册 | M1 |
| POST | /api/auth/login | 登录，返回 JWT | M1 |
| GET  | /api/health | 健康检查 | M1 |
| GET  | /api/nodes | 节点列表 | M1 |
| POST | /api/nodes | 登记节点 | M1 |
| GET  | /api/instances | 实例列表 | M2 |
| POST | /api/instances | 创建实例 | M2 |
| POST | /api/instances/{id}/start | 启动 | M2 |
| POST | /api/instances/{id}/stop | 停止 | M2 |
| POST | /api/instances/{id}/restart | 重启 | M2 |
| GET  | /api/instances/{id}/metrics | 监控数据 | M4 |
| GET  | /api/users | 用户列表（admin）| M3 |
| POST | /api/instances/{id}/assign | 实例授权 | M3 |
| GET  | /api/tunnels | 穿透列表 | M5 |
| POST | /api/tunnels | 分配穿透 | M5 |

---

## WebSocket

| 路径 | 说明 | 阶段 |
|------|------|------|
| /ws/console/{instanceId} | 实例控制台双向流 | M2 |
| /ws/metrics/{instanceId} | 监控数据推送 | M4 |

协议：JSON frame 或二进制，待定（见 ARCHITECTURE.md 控制台细节）。