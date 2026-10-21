# ATL-MCPanel（V1）

Minecraft **多实例管理面板** + 节点控制程序。面板负责用户/权限/实例/备份/告警/穿透，
节点程序（Daemon）跑在每台实例机上负责进程、文件、控制台。

**核心无关**：不绑定任何服务端核心 —— Folia / Paper / Spigot / Vanilla / 任意自定义启动脚本都能管。

> ℹ️ **V1 是完整功能版本，没有"功能阉割"或实例数限制** —— 与后续版本的差别只在
> 节点接入方式与架构，不在 V1 自身的能力。

> ⚠️ **当前状态：V1 开发收尾中，尚未发布正式版本，不建议直接用于生产。**
> 部署流程与已知限制见下方「已知限制」。

> 📄 **许可：MIT**（开源许可，含商业用途）—— 保留版权声明即可自由使用、修改、分发。
> 详见 [许可](#许可) 一节。

---

## 特性

**实例与进程**
- 实例 CRUD、启动 / 停止 / 重启 / **强制关闭**（SIGKILL，含二次确认）
- 实时控制台（xterm.js）+ 500 行历史回放；警告/错误行**整行底色**，异常堆栈续行跟着高亮
- CPU 硬配额与内存上限（**cgroup v2**，实测 20% 配额 → 1.59s / 理论 1.6s）
- 实例目录磁盘用量与软配额（分级告警 + 可选超限自动停机）
- 崩溃补记关机次数、状态自愈（Daemon 上报为准）

**文件与配置**
- 文件管理：浏览 / 编辑 / 重命名 / 复制 / 移动 / 递归搜索 / **流式下载**
- 压缩解压走**节点公共任务队列**（串行消费，避免多个实例抢 IO）
- 配置编辑、启动脚本（`start.sh`）可视可改、核心 jar 管理与切换

**备份**
- 手动 / 定时备份；**保留策略**（梯度 + 手动与自动分开计数）
- 备份日历（自动/手动标记 + 当日明细）；冷存储可指向另一块盘

**玩家**
- 白名单 / OP / 封禁（含 IP）增删查 + 白名单总开关
- **跨来源合并的历史玩家总览**

**穿透（frp）**
- 线路管理 + 每实例公网端口 + 对外域名（给用户看的是域名，而不是节点入口地址）
- **线路自检**：在节点上真的起一个临时 frpc 连一次、占一个端口再撤掉，
  直接区分"地址不可达 / token 不对 / 端口段被占满"
- 面板自身也能穿透发布

**多用户与权限**
- 角色：`admin`（总管理员）/ `nodeuser`（节点用户）/ `user`（普通用户）
- 实例级授权：`owner` / `collab` / `viewer`
- 穿透端口配额按线路发放；**实例 owner 可自助开通端口**（配额记在实例归属者头上）
- 到期时间与到期自动停机；审计日志；公告与帮助文档

**节点与运维**
- 一键部署节点（SSH）、节点监控（CPU/内存/磁盘随心跳上报）
- 告警中心（实例意外停止 / 节点离线 / 磁盘不足 / 备份失败，按类型去重）
- 指标历史（每分钟采样，默认保留 7 天）；面板数据库自动备份

---

## 快速开始

前置：Debian/Ubuntu（x86_64）、Go 1.24+、gcc（SQLite 驱动需要 CGO）、Node 18+；如需穿透再装 frp。

```bash
# 1) 构建后端（脚本会带上版本号等 ldflags，比手敲 go build 更省事）
bash scripts/build.sh

# 2) 构建前端（产物在 web/dist，面板按 server.web_dir 静态托管）
bash scripts/build-web.sh

# 3) 配置：示例文件带注释，复制后按需修改
cp config.example.yaml config.yaml

# 4) 起面板（首次访问会引导创建第一个管理员账号）
./bin/atl-panel -config config.yaml
```

面板默认监听 `:8080`，浏览器打开 `http://<你的IP>:8080` 即可。

> 想要**打包分发**（含 systemd 单元、自签证书、安装脚本）：
> `bash scripts/build-release.sh` 生成 `dist/atlmcpanel-<版本>-<平台>.tar.gz`，
> 解压后执行 `sudo ./deploy/install.sh panel` 即可（配置由脚本自动生成，会保留已有 `config.yaml`）。

节点安装、mTLS 证书、systemd 单元、frps 配置：见 **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)**。

前端开发（热更新）：`cd web && VITE_API_TARGET=http://127.0.0.1:8080 npm run dev`

---

## 已知限制（**请先读**）

| 限制 | 说明 |
|---|---|
| **节点只支持 Linux** | 依赖 cgroup v2、`/proc`、进程组信号；Windows/macOS 上无法运行节点程序 |
| **CPU/内存硬配额需要 cgroup v2**（内核 ≥ 4.15 + 统一层级） | 老内核（如 CentOS 7 的 3.10）上会自动降级并告警，配额不生效 |
| **磁盘配额是软配额** | ext4 没有目录级配额；靠"巡检 + 告警 + 可选停机"兜底，挡不住瞬时写入 |
| **Daemon 重启后实例控制台只读** | 实例的 stdin 由启动它的 Daemon 持有，重启后无法接管输入（可重启实例恢复） |
| **上传单次上限 256MB** | 一次性传输，超大文件会占较多瞬时内存 |
| 单机 / 内网定位 | V1 面向单机或内网多机（通过 SSH 部署节点）；跨公网、NAT 后的多来源节点是后续版本的目标 |

---

## 文档

| 文档 | 内容 |
|---|---|
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | 部署、节点安装、投产前检查清单 |
| [docs/CERTIFICATES.md](docs/CERTIFICATES.md) | 证书生成（自签 / 正式证书替换） |
| [docs/MTLS.md](docs/MTLS.md) | 面板 ↔ 节点 mTLS 说明 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 架构与数据流 |
| [docs/API.md](docs/API.md) | HTTP API 概览 |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | 开发约定、产物平台（多架构/静态编译）、技术接缝与代码规范 |
| [docs/ROADMAP.md](docs/ROADMAP.md) | 里程碑与已完成项 |

---

## 许可

**[MIT License](LICENSE)**（OSI 认可的开源许可）。

| 用途 | 是否许可 |
|---|---|
| 个人学习、研究、自用、开服给朋友玩 | ✅ |
| 学校、社团、公益组织等非营利使用 | ✅ |
| 修改源码自用（含二次开发、二次分发） | ✅ |
| **对外提供收费服务**（含代开服、托管售卖） | ✅ 允许 |
| **随主机/硬件打包售卖、作为商业产品的一部分** | ✅ 允许 |

- 上面只是方便理解的摘要，**以 [LICENSE](LICENSE) 的英文原文为准**。
- MIT 的唯一实质要求：**保留版权声明与许可文本**。
- **变更记录（2026-10-21）**：此前是 PolyForm Noncommercial 1.0.0（仅限非商业）。
  换成 MIT 是为了让项目更容易被采用和二次开发；代价是**不再有"商用需授权"这条限制**。
  已经发布的 `v0.9.19`–`v0.9.22` 仍按当时的 PolyForm 条款发布。

## 贡献

**欢迎提 Issue，暂不接受外部 Pull Request**（为保持版权归属单一）。
Bug、部署问题、文档纠错、使用反馈都很有用 —— 详见 [CONTRIBUTING.md](CONTRIBUTING.md)。

安全漏洞请走 GitHub 的**私有漏洞报告**，不要在公开 Issue 里贴细节。

## 许可与声明

Copyright (c) 2026 ATLCNND (https://github.com/ATLCNND/ATL-MCPanel-V1)

本项目采用 **MIT License**：可以自由使用、修改、分发（**含商业用途**），
唯一要求是**在副本中保留上面的版权声明与许可文本** —— 条款见 [LICENSE](LICENSE)。
因此它同时写在 `LICENSE` 与本 README 里。

分发物里的第三方开源组件（Go 模块、前端依赖、容器基础镜像中的 Debian 组件）
及其完整许可文本，见 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。
