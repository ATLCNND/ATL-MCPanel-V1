# systemd 加固矩阵（面板 vs 节点）

两份单元**刻意不一样**，原因只有一条：**实例进程是 Daemon 的子进程，会继承 Daemon 的沙箱**。
面板只是自己受约束；Daemon 每收紧一项，就等于同时给玩家服换了运行环境。
一个 java 服因为 `PrivateTmp` / `ProtectSystem` 起不来，代价远大于那点加固收益。

| 项 | 面板 | 节点（Daemon） | 理由 |
|---|---|---|---|
| 独立低权用户 | ✅ `User=atlpanel` | ❌ 必须 root | 面板只监听 ≥1024 端口、只代理节点操作；Daemon 要 setuid 拉实例、chown、写 cgroup、调 docker |
| `KillMode` | `control-group`（默认） | **`process`** | 面板重启顺带杀掉自家 frpc 是期望行为；Daemon 重启**绝不能**捎带杀掉玩家服 |
| `NoNewPrivileges` | ✅ | ❌ | Daemon 的实例降权依赖 setuid/setgid |
| `ProtectSystem=strict` | ✅ | ❌ | 实例要写自己的目录；实例目录位置由 `daemon.instance_dir` 决定 |
| `ReadWritePaths` | 只开 `data/` | — | 面板只写数据库 / PKI / 穿透工作目录 / 数据库备份 |
| `ProtectHome` | ✅ | ❌ | 自定义安装可能把实例放在 `/home` 下 |
| `PrivateTmp` | ✅ | ❌ | 实例的 `/tmp` 变成私有目录会跨重启消失 |
| `PrivateDevices` | ✅ | ❌ | 容器/实例可能要用 `/dev` 下的设备 |
| `ProtectKernelTunables` / `Modules` | ✅ | ❌ | 实例可能读 `/proc/sys`（如 `boot_id`、`random/entropy_avail`） |
| `ProtectControlGroups` | ✅ | ❌ | Daemon 自己要写 cgroup 的 cpu/memory 配额 |
| `RestrictAddressFamilies` | TCP/UDP + unix + netlink | ❌ | java 与脚本会用到 `AF_PACKET` / `AF_VSOCK` 等 |
| `RestrictNamespaces` | ✅ | ❌ | Daemon 要建容器网络命名空间 |
| `RestrictRealtime` / `LockPersonality` / `RestrictSUIDSGID` | ✅ | 部分（`RestrictRealtime`、`LockPersonality`） | 这几项只约束 Daemon 自身的调度与身份操作，不影响实例读写 |
| `SystemCallFilter=@system-service` | ✅ | ❌ | 玩家服跑的是任意第三方 jar |
| `ProtectClock` | — | ✅ | Daemon 不需要改系统时钟 |

## 面板不再是 root 之后，哪些功能会受影响

面板的**「本机一键部署节点」**要写 `/etc/systemd/system/atlmcpanel-daemon.service` 并调
`systemctl enable --now` —— 这两件都需要 root。加固模式下它会失败，界面里会明确报权限错误。

两条出路：

1. **推荐**：节点用节点包在那台机器上跑 `sudo ./install.sh daemon`，面板这边只做「远程登记」
   （SSH / 凭证交换 / 粘贴导入都不需要 root）。
2. 单机图省事：安装面板时显式退回老行为 —— `PANEL_USER=root sudo ./install.sh panel`。

## 面板沙箱的边界（真机验证过的）

- 只写 `data/`（SQLite、`data/pki`、`data/panel-frp`、`data/backups`）；其它位置一律只读。
- HTTPS 证书与 mTLS 的 CA 通过 `certs/` **只读**挂进去（`ReadOnlyPaths`），所有权归面板用户。
- `PrivateTmp` 生效后，面板创建的 frpc 配置文件与日志都在 `data/panel-frp` 下，与 `/tmp` 无关。
- `SystemCallFilter=@system-service` 覆盖 Go 运行时的全部系统调用；如果以后面板要
  `unshare` / `mount` / `ptrace` 之类，需要在这里补白名单（**别直接删掉这一行**）。
