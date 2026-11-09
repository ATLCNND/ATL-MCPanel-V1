# 面板容器化部署（Docker）

把**面板**跑在容器里。节点（Daemon）仍然装在宿主机或别的机器上 ——
Daemon 要以 root 拉起实例、写 cgroup、调 docker CLI，塞进容器只会多一层麻烦。

> 这是 V1.0 preview 四项门槛里 D 项的一半：**面板容器化包**。
> 另一半是 systemd 加固，见 `deploy/systemd/HARDENING.md`。

## 三步

```bash
# 1) 构建镜像（需要仓库里已有 bin/…/atl-panel 与 web/dist）
./deploy/docker/build.sh --tag atl-mcpanel-panel:0.11.3

# 2) 准备挂载点
sudo mkdir -p /srv/mcpanel/{data,certs}
sudo cp config.example.yaml /srv/mcpanel/config.yaml
sudo chown -R 10001:10001 /srv/mcpanel/data /srv/mcpanel/certs
# config.yaml 是**容器里的进程**读的，必须让 10001 能读：
# 只 chmod 600 而属主是 root，容器会以「permission denied」直接退出（真机踩过）。
sudo chown 10001:10001 /srv/mcpanel/config.yaml
sudo chmod 640 /srv/mcpanel/config.yaml

# 3) 起来
ATL_ROOT=/srv/mcpanel ATL_CERTS=/srv/mcpanel/certs ATL_CONFIG=/srv/mcpanel/config.yaml \
  docker compose -f deploy/docker/docker-compose.yml up -d

# 首个管理员（面板不提供免登录注册入口，只能本机 CLI 建）
docker exec -it atl-mcpanel-panel /opt/mcpanel/bin/atl-panel \
  -config /opt/mcpanel/config.yaml user create -username admin -role admin -if-empty
```

## 为什么用 host 网络

面板要连节点 Daemon，默认是 `127.0.0.1:9091`。

- 桥接网络下「容器 → 宿主 127.0.0.1」到不了宿主；
- 节点的 `atl-container-firewall` 单元会 **DROP 容器网段到宿主的新连接**
  （`-i br-+ --ctstate NEW -j DROP`），这是刻意的隔离 —— 不该为了面板开一个洞。

`network_mode: host` 下容器与宿主共享网络命名空间，9091 直接可达，也不用映射端口。
代价是 `ports:` 在 host 模式下无效：**面板监听哪个端口完全由 `config.yaml` 决定**。
健康检查（`healthcheck.sh`）会自己从挂载进来的 `config.yaml` 里读 `server.listen`
与 `server.security_entry`，所以改端口/加入口**不用**同步改 compose；
`ATL_PORT` / `ATL_ENTRY` 只在配置文件读不到时兜底。

面板与节点不在同一台机器时，可以改回桥接 + `ports:`，但要自己保证：

- `server.listen` / `server.grpc_listen` 绑定可达地址，不是 `127.0.0.1`；
- 节点的 `daemon.panel_address` 指向宿主 IP。

## 镜像里的约束（与 systemd 加固是同一套设计）

| 项 | 值 | 为什么 |
|---|---|---|
| 运行用户 | UID/GID **10001**（数字，非名字） | 宿主上不存在 `atlpanel` 用户，写名字会起不来；数字 ID 才能用 `chown` 对上 |
| 根文件系统 | `read_only: true` + `/tmp` 为 tmpfs | 面板只写 `data/` 与 `certs/`，与 `ProtectSystem=strict` 等价 |
| capabilities | `cap_drop: ALL` | 面板不需要任何特权（不碰实例、不改宿主） |
| 可写挂载 | `data/`、`certs/` | `data/` = SQLite/PKI/穿透工作目录/数据库备份；`certs/` = 证书页会写它 |
| `config.yaml` | `:ro` | 面板**从不**回写配置，改端口要改文件再重启容器 |

## 已知限制

1. **「本机一键部署节点」不可用。** 那需要写 `/etc/systemd/system` 并调 `systemctl`，
   容器里两者都没有。请在那台机器上用节点包执行 `deploy/install.sh daemon`，
   再回到面板用 SSH / 凭证交换 / 粘贴导入登记。
2. **面板穿透需要镜像里有 `frpc`。** `build.sh` 会带上 `bin/frpc`（若存在）；
   没有它只是穿透不可用，面板不会崩。
3. **健康检查读的是挂载进来的配置。** 它从 `config.yaml` 里取 `server.listen`
   的端口与 `server.security_entry` 的前缀；只有配置文件读不到时才用
   `ATL_PORT` / `ATL_ENTRY` 兜底。`config.yaml` 必须对 UID 10001 可读（见上面第 2 步）。
4. 离线环境（构建机访问不了 Docker Hub）：
   `BASE=atl-mcpanel-runtime:latest ./deploy/docker/build.sh`
   —— 官方二进制是完全静态链接的，基底只要有 `/bin/sh` 就能跑。

## 为什么不把 Daemon 也容器化

Daemon 要做的事天然和容器相冲：以 `daemon.instance_user` 身份 setuid 拉实例、
写 cgroup 的 cpu/memory 配额、chown 实例目录、调 docker CLI 起嵌套容器。
把它关进容器，等于要么给它 `--privileged`（那不如直接跑在宿主上），
要么把宿主的一堆东西挂进去（cgroup、docker.sock、实例目录），最后得到的是
一个「看起来隔离、实际全通」的东西。

节点的加固走的是另一条路：**systemd 单元只加不影响实例的项**，
逐条取舍写在 `deploy/systemd/HARDENING.md`。
