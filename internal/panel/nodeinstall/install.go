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
		dir = "/opt/mcpanel"
	}
	svc := o.ServiceName
	if svc == "" {
		svc = "atlmcpanel-daemon"
	}
	res := &Result{Steps: []string{}}
	addStep := func(s string) { res.Steps = append(res.Steps, s) }

	// 1. 创建目录结构
	if _, err := c.Run(fmt.Sprintf("mkdir -p %s/bin %s/instances %s/certs %s/data", dir, dir, dir, dir)); err != nil {
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}
	addStep("创建目录 " + dir)

	// 2. 上传 Daemon 二进制
	if err := c.Upload(dir+"/bin/dsh-daemon", o.DaemonBinary, 0o755); err != nil {
		return nil, fmt.Errorf("上传 Daemon 二进制失败: %w", err)
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

	res.Message = "Daemon 部署完成并已启动"
	if useTLS {
		res.Message += "（mTLS 已启用）"
	}
	return res, nil
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
	fmt.Fprintf(&sb, "  instance_dir: %q\n", dir+"/instances")
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
