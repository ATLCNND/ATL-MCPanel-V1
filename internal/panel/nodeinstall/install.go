// Package nodeinstall 通过 SSH 在远程节点上部署 / 管理 Daemon。
//
// 流程：
//  1. 探测节点环境（架构、sudo 权限、目录、是否已安装 Daemon）
//  2. 上传 Daemon 二进制 + 配置 + mTLS 证书
//  3. 安装 systemd 单元并启动
//
// 设计考虑：
//   - 所有步骤幂等，可重复执行（升级 = 重新上传并重启）
//   - 二进制由面板本地二进制同目录提供（dsh-daemon），确保版本一致
//   - 凭据仅用于本次连接，不落盘
package nodeinstall

import (
	"fmt"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Options 部署参数。
type Options struct {
	Host      string // 节点 IP / 主机名
	Port      int    // SSH 端口
	User      string // SSH 用户
	Auth      string // 密码或私钥内容（以 PRIVATE KEY 开头则视为密钥）
	RemoteDir string // 安装目录，默认 /opt/mcpanel

	// Daemon 配置
	NodeID       string
	PanelAddress string // Panel gRPC 地址（节点可访问的地址:端口）
	DaemonBinary []byte // dsh-daemon 二进制内容
	GRPCListen   string // Daemon 反向 gRPC 监听，默认 ":9091"
	ServiceName  string // systemd 单元名，默认 atlmcpanel-daemon

	// FrpcBinary 穿透客户端（可选）。有就一并下发 —— Daemon 用 exec.LookPath("frpc")
	// 找它，节点上没有 frpc 时"给实例开公网端口"整块不可用，而让用户自己去 GitHub
	// 下 frp 完全没有必要。为空时跳过并只提示，不算失败（穿透是可选能力）。
	FrpcBinary []byte

	// InstanceDir 节点上的实例目录（留空 = 沿用节点现有配置里的值；
	// 节点上还没有配置时退回 <安装目录>/instances）。见 reuseInstanceDir。
	InstanceDir string

	// mTLS 材料（可为空表示明文）
	CACert     []byte
	ClientCert []byte
	ClientKey  []byte
}

// Result 部署结果。
type Result struct {
	Steps   []string `json:"steps"`
	Version string   `json:"version"`
	Message string   `json:"message"`
}

// Client SSH 客户端。
type Client struct {
	conn *ssh.Client
}

// Dial 建立 SSH 连接。
func Dial(o Options) (*Client, error) {
	if o.Host == "" {
		return nil, fmt.Errorf("节点地址不能为空")
	}
	port := o.Port
	if port <= 0 {
		port = 22
	}
	user := o.User
	if user == "" {
		user = "root"
	}

	var auths []ssh.AuthMethod
	if strings.Contains(o.Auth, "PRIVATE KEY") {
		signer, err := ssh.ParsePrivateKey([]byte(o.Auth))
		if err != nil {
			return nil, fmt.Errorf("解析 SSH 私钥失败: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	} else {
		auths = append(auths, ssh.Password(o.Auth))
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 与面板现有节点登记模型一致（凭据可信）
		Timeout:         15 * time.Second,
	}

	addr := net.JoinHostPort(o.Host, strconv.Itoa(port))
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Close 关闭连接。
func (c *Client) Close() error { return c.conn.Close() }

// Run 执行命令并返回合并输出。
func (c *Client) Run(cmd string) (string, error) {
	if c == nil || c.conn == nil {
		return "", fmt.Errorf("SSH 连接未建立")
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// Probe 探测节点环境。
type Probe struct {
	Arch          string `json:"arch"`
	OS            string `json:"os"`
	HasSystemd    bool   `json:"has_systemd"`
	Installed     bool   `json:"installed"`
	ExistingSvc   bool   `json:"existing_service"`
	BinaryVersion string `json:"binary_version"`
	FreeDiskMB    int64  `json:"free_disk_mb"`
}

// Do 探测节点环境。
func (c *Client) Probe(remoteDir string) (*Probe, error) {
	p := &Probe{}

	if out, err := c.Run("uname -m"); err == nil {
		p.Arch = strings.TrimSpace(out)
	}
	if out, err := c.Run("cat /etc/os-release 2>/dev/null | grep ^PRETTY_NAME | cut -d'\"' -f2"); err == nil {
		p.OS = strings.TrimSpace(out)
	}
	if out, err := c.Run("command -v systemctl >/dev/null 2>&1 && echo yes || echo no"); err == nil {
		p.HasSystemd = strings.TrimSpace(out) == "yes"
	}
	if out, err := c.Run(fmt.Sprintf("test -x %s/bin/dsh-daemon && echo yes || echo no", remoteDir)); err == nil {
		p.Installed = strings.TrimSpace(out) == "yes"
	}
	if out, err := c.Run("test -f /etc/systemd/system/atlmcpanel-daemon.service && echo yes || echo no"); err == nil {
		p.ExistingSvc = strings.TrimSpace(out) == "yes"
	}
	if out, err := c.Run(fmt.Sprintf("%s/bin/dsh-daemon -version 2>/dev/null || echo unknown", remoteDir)); err == nil {
		p.BinaryVersion = strings.TrimSpace(out)
	}
	// 安装目录可能尚不存在，需回溯到最近的存在目录再统计可用空间
	dfCmd := fmt.Sprintf(
		"D=%s; while [ ! -d \"$D\" ] && [ \"$D\" != \"/\" ]; do D=$(dirname \"$D\"); done; df -Pm \"$D\" | tail -1 | awk '{print $4}'",
		shellQuote(remoteDir))
	if out, err := c.Run(dfCmd); err == nil {
		p.FreeDiskMB, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	return p, nil
}

// Upload 通过 SFTP 上传内容（自动创建父目录）。
func (c *Client) Upload(remotePath string, content []byte, mode os.FileMode) error {
	sc, err := sftp.NewClient(c.conn)
	if err != nil {
		return fmt.Errorf("建立 SFTP 会话失败: %w", err)
	}
	defer sc.Close()

	dir := path.Dir(remotePath)
	if _, err := c.Run("mkdir -p " + shellQuote(dir)); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	f, err := sc.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("打开远程文件失败: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("写入远程文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭远程文件失败: %w", err)
	}
	// SFTP 无法直接设置权限位，用 chmod 补齐
	if _, err := c.Run(fmt.Sprintf("chmod %o %s", mode.Perm(), shellQuote(remotePath))); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	return nil
}

// Deploy 执行完整部署流程。
func (c *Client) Deploy(o Options) (*Result, error) {
	// 先做本地校验（不依赖连接），避免无效参数走到一半才失败
	if len(o.DaemonBinary) == 0 {
		return nil, fmt.Errorf("缺少 Daemon 二进制内容")
	}
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("SSH 连接未建立")
	}

	dir := o.RemoteDir
	if dir == "" {
		// 与 config 的默认保持一致：**独立于面板目录**。
		// 用面板目录会让节点安装重写面板自己的 config.yaml（丢掉 TLS/端口设置）。
		dir = "/opt/atl-node"
	}
	svc := o.ServiceName
	if svc == "" {
		svc = "atlmcpanel-daemon"
	}
	res := &Result{Steps: []string{}}
	addStep := func(s string) { res.Steps = append(res.Steps, s) }

	// 0. 架构校验（放在最前面）
	//
	// 面板只能下发"自己同目录那一份" dsh-daemon，其架构在编译时固定。
	// 不先比对的话，arm64 节点会被装上一个 amd64 二进制，直到启动才报
	// Exec format error —— 那个错误离真正原因隔了好几层。
	// 这里提前失败，并直接告诉用户该用哪个架构的包。
	if err := c.checkArch(o.DaemonBinary); err != nil {
		return nil, err
	}

	// 1. 创建目录结构
	if _, err := c.Run(fmt.Sprintf("mkdir -p %s/bin %s/instances %s/certs %s/data", dir, dir, dir, dir)); err != nil {
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}
	addStep("创建目录 " + dir)
	// 面板目录与节点目录相同时给出**显式警告**：这不是不能跑，而是同机部署时
	// 节点安装会覆盖面板的 config.yaml（丢 TLS/端口），现象与原因隔得很远。
	if dir == "/opt/mcpanel" {
		addStep("⚠️ 节点目录与面板目录相同：节点安装会覆盖面板的 config.yaml，建议改成 /opt/atl-node")
	}

	// 2. 上传 Daemon 二进制
	//
	// 上传前**再确保一次目录存在**：SFTP 打开远程文件失败（sftp: "Failure"）
	// 最常见的原因就是目录还没建好（或建在了另一个路径上），而那时错误信息里
	// 只有一句 "打开远程文件失败"，很难联想到目录 —— 重试一次比让人去猜便宜。
	if err := c.Upload(dir+"/bin/dsh-daemon", o.DaemonBinary, 0o755); err != nil {
		_, _ = c.Run(fmt.Sprintf("mkdir -p %s/bin && chmod 755 %s %s/bin", dir, dir, dir))
		if err2 := c.Upload(dir+"/bin/dsh-daemon", o.DaemonBinary, 0o755); err2 != nil {
			return nil, fmt.Errorf("上传 Daemon 二进制失败: %w（已自动建目录后重试仍失败）", err)
		}
	}
	addStep("上传 Daemon 二进制")

	// 3. 上传 mTLS 材料
	useTLS := len(o.CACert) > 0 && len(o.ClientCert) > 0 && len(o.ClientKey) > 0
	if useTLS {
		if err := c.Upload(dir+"/certs/ca.crt", o.CACert, 0o644); err != nil {
			return nil, fmt.Errorf("上传 CA 证书失败: %w", err)
		}
		if err := c.Upload(dir+"/certs/node.crt", o.ClientCert, 0o644); err != nil {
			return nil, fmt.Errorf("上传节点证书失败: %w", err)
		}
		if err := c.Upload(dir+"/certs/node.key", o.ClientKey, 0o600); err != nil {
			return nil, fmt.Errorf("上传节点私钥失败: %w", err)
		}
		addStep("上传 mTLS 证书")
	}

	// 4. 写配置
	//
	// 实例目录：**优先沿用节点上已有的那一份**（见 reuseInstanceDir 的注释）——
	// 重跑一次一键部署不该把在跑的实例"换到另一个目录去"，
	// 那样面板与 Daemon 会对不上（现象是实例还在跑、面板里却是空的/未注册）。
	if o.InstanceDir == "" {
		o.InstanceDir = reuseInstanceDir(c, dir)
	}
	cfg := renderDaemonConfig(o, dir, useTLS)
	if err := c.Upload(dir+"/config.yaml", []byte(cfg), 0o600); err != nil {
		return nil, fmt.Errorf("写入配置失败: %w", err)
	}
	addStep("写入 config.yaml")

	// 5. 安装 systemd 单元
	unitPath := "/etc/systemd/system/" + svc + ".service"
	if err := c.Upload(unitPath, []byte(systemdUnit(dir, svc)), 0o644); err != nil {
		return nil, fmt.Errorf("写入 systemd 单元失败: %w", err)
	}
	addStep("安装 systemd 单元 " + svc)

	// 6. 重载并启动
	if out, err := c.Run(fmt.Sprintf("systemctl daemon-reload && systemctl enable --now %s 2>&1", svc)); err != nil {
		return nil, fmt.Errorf("启动服务失败: %s (%v)", out, err)
	}
	addStep("启动 " + svc + " 服务")

	// 7. 校验
	time.Sleep(3 * time.Second)
	out, _ := c.Run("systemctl is-active " + svc)
	status := strings.TrimSpace(out)
	if status != "active" {
		logs, _ := c.Run(fmt.Sprintf("journalctl -u %s --no-pager -n 15", svc))
		return nil, fmt.Errorf("Daemon 未能正常运行（状态 %s）:\n%s", status, logs)
	}
	addStep("服务状态校验通过")

	// 8. 容器隔离防火墙（**容器化可用的前提**，不是可选加固）
	//
	// 为什么放在节点部署里：这条 iptables 规则是"容器不得访问宿主服务"的唯一保证，
	// 少了它，实例能连到宿主上绑 0.0.0.0 的服务（实测能连上 Daemon 的 gRPC 端口），
	// 容器隔离就只剩一半。此前只有"节点包 + install.sh"那条路径会装它，
	// 走面板一键部署的机器会**静默缺少**这条规则 —— 属于安全项，不能靠文档提醒。
	//
	// 做成 systemd 单元而不是直接加规则：直接加只在当下生效，服务器一重启隔离就悄悄失效；
	// 单元里 After=docker.service 保证规则落在 docker 重建自己的链之后，且脚本是幂等的。
	if out, err := installContainerFirewall(c); err != nil {
		// 不因为这一步失败就判定部署失败：没装 docker 的节点本来就用不上容器化。
		addStep("⚠️ 容器隔离防火墙未安装：" + firstLine(out))
	} else {
		addStep("安装容器隔离防火墙（容器不得访问宿主服务）")
	}

	// 9. 穿透客户端 frpc（可选：Daemon 用 exec.LookPath("frpc") 找它）
	//
	// 面板包与节点包同目录时（一键部署下发的是面板包里那份），这里把 frpc 一并推过去，
	// 让"穿透"开箱可用；没有 frpc 时只提示、不算失败（穿透是可选能力）。
	if len(o.FrpcBinary) > 0 {
		if err := c.Upload(dir+"/bin/frpc", o.FrpcBinary, 0o755); err != nil {
			addStep("⚠️ frpc 上传失败：" + err.Error())
		} else {
			// LookPath 只看 PATH，而 systemd 的默认 PATH 含 /usr/local/bin ——
			// 少了这一步会出现"包里有 frpc、Daemon 却说找不到"这种很难查的不一致。
			if _, err := c.Run("install -m755 " + dir + "/bin/frpc /usr/local/bin/frpc"); err != nil {
				addStep("⚠️ frpc 已上传但未能链接到 /usr/local/bin")
			} else {
				addStep("安装穿透客户端 frpc")
			}
		}
	}

	res.Message = "Daemon 部署完成并已启动"
	if useTLS {
		res.Message += "（mTLS 已启用）"
	}
	return res, nil
}

// containerFirewallUnit 容器隔离防火墙的 systemd 单元（与节点包里那份保持一致）。
//
// 直接内联而不是"从包里读文件"：一键部署可能运行在一个只有面板二进制的环境里，
// 少一个文件依赖就少一种"部署成功、隔离没生效"的可能。
//
// ⚠️ 规则必须限定 `--ctstate NEW`（2026-10-01 内测踩到，代价很大）：
// 原来是无条件 `-i br-+ -j DROP`，它连**宿主自己发起的**到容器的连接的回程也一起丢了 ——
// docker-proxy 用网桥地址（172.18.0.1）作源去连容器，容器的 SYN-ACK 目的地址就是宿主的
// 网桥地址 → 命中 INPUT 链 → 被丢弃，连接永远停在 SYN_RECV。后果是
// **`-p 127.0.0.1:25565:25565` 这种端口发布整个失效**：容器端口在宿主上连不通，
// frpc 也就转不进去，外面用 MOTD 工具查实例一律超时。
//
// 而它**看起来**是好的：TCP 连接能连上（docker-proxy 会先 accept），
// 只是永远没有数据回来 —— 现象与"服务端没开 enable-status"极像，很容易查错方向。
//
// 限定 NEW 之后语义才正确：容器**主动发起**到宿主服务的连接被挡（我们的目标），
// 宿主主动连容器的回程属于 ESTABLISHED，照常放行。
const containerFirewallUnit = `[Unit]
Description=ATL-MCPanel 容器隔离防火墙规则（容器不得访问宿主服务）
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
# 幂等：先 -C 检查，存在就跳过；重复执行不会堆叠规则。
# 只挡 NEW：容器主动发起的连接被挡，宿主主动连容器的回程（ESTABLISHED）必须放行，
# 否则 docker 的端口发布就废了（见 install.go 里这段注释的完整说明）。
ExecStart=/bin/bash -c 'iptables -C INPUT -i br-+ -m conntrack --ctstate NEW -j DROP 2>/dev/null || iptables -I INPUT -i br-+ -m conntrack --ctstate NEW -j DROP; iptables -C INPUT -i docker0 -m conntrack --ctstate NEW -j DROP 2>/dev/null || iptables -I INPUT -i docker0 -m conntrack --ctstate NEW -j DROP'
# 停止时不删规则：它们是安全控制，服务停了也不该把口子放开
ExecStop=/bin/true

[Install]
WantedBy=multi-user.target
`

// installContainerFirewall 安装并启用容器隔离防火墙单元；返回命令输出（供失败提示用）。
//
// 没有 docker 时**不装**：规则本身没坏处，但没有容器就没有意义，
// 而且部分发行版连 iptables 都没装 —— 那种情况下"单元启动失败"会让人误以为部署有问题。
func installContainerFirewall(c *Client) (string, error) {
	if out, err := c.Run("command -v iptables >/dev/null 2>&1 && command -v docker >/dev/null 2>&1"); err != nil {
		return "节点上没有 docker 或 iptables（容器化不可用，跳过）", nil
	} else if strings.TrimSpace(out) != "" {
		return out, nil
	}
	const path = "/etc/systemd/system/atl-container-firewall.service"
	if err := c.Upload(path, []byte(containerFirewallUnit), 0o644); err != nil {
		return "写入单元失败: " + err.Error(), err
	}
	out, err := c.Run("systemctl daemon-reload && systemctl enable --now atl-container-firewall 2>&1 && " +
		"iptables -S INPUT | grep -cE '\\-i (br-\\+|docker0)'")
	if err != nil {
		return out, err
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if strings.TrimSpace(s) == "" {
		return "（无输出）"
	}
	return s
}

// reuseInstanceDir 读节点上**已有的** daemon 配置，沿用它的 instance_dir。
//
// 为什么必须这么做（2026-09-30 在内测节点上踩到）：一键部署会重写节点配置，
// 而配置里的 instance_dir 原先是"安装目录 + /instances"。于是**重跑一次部署**
// 就把实例目录换了个地方 —— 实例进程照常在跑（旧目录里），新 Daemon 却去新目录找，
// 结果是"面板里实例消失/变成未注册"，而机器上什么都没坏。同机部署时这个坑尤其致命：
// 面板自己的实例就在它的目录下。
//
// 读不到（首次部署）或读不出值时返回空串，由调用方退回默认。
func reuseInstanceDir(c *Client, dir string) string {
	out, err := c.Run(fmt.Sprintf("grep -m1 '^\\s*instance_dir:' %s/config.yaml 2>/dev/null", dir))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return ""
	}
	// 形如：  instance_dir: "/opt/mcpanel/instances"（或单引号 / 无引号）
	if i := strings.Index(line, ":"); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	line = strings.Trim(line, `"'`)
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "{{") {
		return ""
	}
	return line
}

// renderDaemonConfig 生成节点配置。
func renderDaemonConfig(o Options, dir string, useTLS bool) string {
	var sb strings.Builder
	sb.WriteString("# 由 ATL-MCPanel 自动生成\n")
	sb.WriteString("server:\n  listen: \":8080\"\n\n") // Daemon 不使用 HTTP，占位以满足统一配置结构

	sb.WriteString("daemon:\n")
	fmt.Fprintf(&sb, "  node_id: %q\n", o.NodeID)
	fmt.Fprintf(&sb, "  panel_address: %q\n", o.PanelAddress)
	grpcListen := o.GRPCListen
	if grpcListen == "" {
		grpcListen = ":9091"
	}
	fmt.Fprintf(&sb, "  grpc_listen: %q\n", grpcListen)
	// 实例目录：沿用节点已有配置（同机部署时就是面板自己的实例目录），
	// 否则退回 <安装目录>/instances。state/resource/frp 目录由 Daemon 从它推导。
	if o.InstanceDir != "" {
		fmt.Fprintf(&sb, "  instance_dir: %q\n", o.InstanceDir)
	} else {
		fmt.Fprintf(&sb, "  instance_dir: %q\n", dir+"/instances")
	}
	if useTLS {
		sb.WriteString("  tls: true\n")
		fmt.Fprintf(&sb, "  cert_file: %q\n", dir+"/certs/node.crt")
		fmt.Fprintf(&sb, "  key_file: %q\n", dir+"/certs/node.key")
		fmt.Fprintf(&sb, "  ca_file: %q\n", dir+"/certs/ca.crt")
	} else {
		sb.WriteString("  tls: false\n  cert_file: \"\"\n  key_file: \"\"\n  ca_file: \"\"\n")
	}
	return sb.String()
}

// systemdUnit 生成 Daemon 的 systemd 单元。
// KillMode=process 很关键：默认的 control-group 会在服务重启时连带杀死
// Minecraft 子进程，导致实例意外中断。
func systemdUnit(dir, svc string) string {
	return fmt.Sprintf(`[Unit]
Description=ATL-MCPanel Daemon (%s)
After=network.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s/bin/dsh-daemon -config %s/config.yaml
Restart=on-failure
RestartSec=5
KillMode=process
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`, svc, dir, dir, dir)
}

// shellQuote 单引号转义，防止路径中的特殊字符被 shell 解释。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
