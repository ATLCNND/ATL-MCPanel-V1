# 面板加固与容器化部署

> 底账 S9 / 1.0.0-preview 的 D 项。目标不是「把面板关进笼子到不能用」，
> 而是**在功能不受影响的前提下，缩小它被打穿之后的破坏范围**。

---

## 1. 两种部署形态

| 形态 | 适用 | 加固手段 |
|---|---|---|
| **systemd 单元**（默认） | 有自己的机器、要管实例与本机节点 | 内核级沙箱开关（见下） |
| **容器**（`deploy/docker/`） | 只要面板、实例都在别的节点上 | 非 root + 只读根 + 丢全部 capability |

两者可以并存：容器跑面板，systemd 跑节点守护进程。

---

## 2. 面板的 systemd 加固

`deploy/systemd/atlmcpanel-panel.service` 里已开启：

| 开关 | 作用 |
|---|---|
| `NoNewPrivileges=true` | 禁止通过 setuid 等方式提权 —— 拿到一个 RCE 也拿不到更多权限 |
| `ProtectSystem=strict` + `ReadWritePaths=<安装目录>` | 整个文件系统只读，**只允许写安装目录** |
| `ProtectHome=true` | 看不到 `/home` 与 `/root` |
| `PrivateTmp=true` / `PrivateDevices=true` | 独立的 `/tmp` 与 `/dev` |
| `ProtectKernelTunables/Modules/Logs` | 不许碰内核参数、模块、日志 |
| `ProtectControlGroups=true` | 不许改 cgroup 树 |
| `ProtectClock` / `ProtectHostname` | 不许改系统时间与主机名 |
| `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX` | 只留面板真正用到的协议族 |
| `RestrictSUIDSGID` / `LockPersonality` / `RestrictNamespaces` | 不许建 setuid 文件、改执行域、建命名空间 |
| `RestrictRealtime=true` | 不许用实时调度抢满 CPU |
| `SystemCallArchitectures=native` | 只允许本机架构的系统调用 |

### 节点守护进程的加固刻意更宽松

`atlmcpanel-daemon.service` 只开了其中一部分（`NoNewPrivileges`、`ProtectHome`、
`ProtectHostname`、`ProtectClock`、`ProtectKernelLogs`、`RestrictRealtime`、`RestrictSUIDSGID`），
**没有**开 `ProtectSystem` / `PrivateTmp` / `RestrictNamespaces` / `PrivateDevices`，原因写在单元文件里：

- 它要 `useradd` 建实例专用用户 → 必须能写 `/etc/passwd` 一类账号文件
- 它要 exec docker、给实例 chown → 锁死根文件系统或命名空间会让建实例直接失败
- 用户的启动脚本可能引用宿主 `/tmp` → `PrivateTmp` 会造成意外失败

> `KillMode=process` **不能改**：改成 `control-group` 会在守护进程重启时连实例进程一起杀掉，
> 玩家掉线、存档可能损坏。这一条与加固无关，是正确性要求。

---

## 3. 容器化部署

`deploy/docker/Dockerfile.panel` + `docker-compose.yml`：

```bash
# 在解压后的面板包目录里
docker build -f deploy/docker/Dockerfile.panel -t atl-mcpanel-panel:0.12 .
docker compose -f deploy/docker/docker-compose.yml up -d
```

镜像的取向：

- 基于 `debian:bookworm-slim`，只装 `ca-certificates` 与 `tzdata`（不带 shell 工具链）
- **以非 root 用户运行**（uid 10001）—— 面板本身不需要 root
- 容器内不加 `privileged`、不挂 `docker.sock`、丢全部 capability、根文件系统只读
- 数据全走卷：`config.yaml`（只读）、`data`（数据库与证书）
- 端口默认只发布到 `127.0.0.1`；**直接裸奔暴露到公网不是推荐做法**，前面请放 HTTPS

---

## 4. 真机验证记录（2026-10-25）

**systemd 加固（VM，0.12.8）**

```
面板: NoNewPrivileges=yes  ProtectSystem=strict  ReadWritePaths=/opt/mcpanel  PrivateTmp=yes
守护: NoNewPrivileges=yes  ProtectHome=yes  KillMode=process（未被改动）
```

加固后功能复验：健康检查 200、节点列表、实例列表（9 条）、meta、证书状态接口全部正常；
并在加固状态下成功**创建一个 docker 模板实例并启动**（200 / 0.2 秒）——
说明 `useradd`、`chown`、exec docker 都没有被沙箱挡住。

**容器化（公网节点，0.12.8）**

```
镜像: atl-mcpanel-panel:0.12  124MB
容器: Up   127.0.0.1:25081->25080/tcp
探测: HTTP/1.1 200 OK（/api/health）
加固: ReadonlyRootfs=true  CapDrop=[ALL]  no-new-privileges  User=10001:10001
```

> 已知限制：镜像里没有 `curl`/`wget`，所以 `HEALTHCHECK` 用 `atl-panel -version` 作为存活代理；
> 它只能证明进程还在、二进制可执行，**不能**证明 HTTP 端口在服务。
> 若需要真实探活，请在自带 curl 的基础上自行改 HEALTHCHECK，或用外部监控。
