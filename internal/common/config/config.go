// Package config 提供统一的配置加载。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 顶层配置结构。
type Config struct {
	Server ServerConfig `yaml:"server"`
	Daemon DaemonConfig `yaml:"daemon"`
	DB     DBConfig     `yaml:"db"`
	Auth   AuthConfig   `yaml:"auth"`
}

// ServerConfig Panel HTTP/gRPC 服务配置。
type ServerConfig struct {
	Listen      string `yaml:"listen"`       // HTTP 监听地址，如 ":8080"
	GRPCListen  string `yaml:"grpc_listen"`  // gRPC 监听地址，如 ":9090"
	ExternalURL string `yaml:"external_url"` // 外部访问地址（用于生成穿透等）
	WebDir      string `yaml:"web_dir"`      // 前端静态资源目录（SPA），为空则不托管

	// HTTPS 配置
	// 若设置 tls_listen：HTTPS 监听该地址，同时保留 listen 的 HTTP（双端口模式）
	// 若仅设置 tls_cert/tls_key：listen 端口直接以 HTTPS 提供服务（单端口模式）
	// 证书可替换为阿里云/其它 CA 签发的 Nginx 格式证书（.pem + .key）
	TLSListen string `yaml:"tls_listen"`
	TLSCert   string `yaml:"tls_cert"`
	TLSKey    string `yaml:"tls_key"`

	// TrustProxy 面板位于 frp / 反向代理之后时开启：
	// 客户端 IP 取 X-Forwarded-For 首项，否则所有请求都会记作代理地址
	TrustProxy bool `yaml:"trust_proxy"`

	// GRPCMTLS 要求 Daemon 使用由面板 CA 签发的客户端证书接入（双向认证）。
	// 生产环境应保持开启；关闭仅用于从明文平滑迁移。
	GRPCMTLS bool `yaml:"grpc_mtls"`
	// PKIDir CA 与签发证书的存放目录
	PKIDir string `yaml:"pki_dir"`

	// GRPCPublicAddress 各节点用于连接面板 gRPC 的地址（host:port）。
	// 一键部署节点时写入节点配置；留空则尝试从 external_url 推导。
	GRPCPublicAddress string `yaml:"grpc_public_address"`
	// DaemonBinary 节点部署时下发的 Daemon 二进制路径。
	// 留空则使用与面板二进制同目录的 dsh-daemon（保证版本一致）。
	DaemonBinary string `yaml:"daemon_binary"`

	// 一键部署到节点时使用的目录与单元名（一般无需修改；
	// 同机运行多个 Daemon 时可借此区分）
	RemoteInstallDir  string `yaml:"remote_install_dir"`
	DaemonServiceName string `yaml:"daemon_service_name"`
	DaemonGRPCListen  string `yaml:"daemon_grpc_listen"`

	// 面板自身穿透的工作目录（存放 panel-frpc 配置与日志）
	PanelFrpDir string `yaml:"panel_frp_dir"`
}

// DaemonConfig Daemon 连接 Panel 的配置。
type DaemonConfig struct {
	NodeID       string `yaml:"node_id"`
	PanelAddress string `yaml:"panel_address"` // Panel gRPC 地址
	GRPCListen   string `yaml:"grpc_listen"`   // Daemon 自身 gRPC 监听（供 Panel 反向调用），如 ":9091"
	InstanceDir  string `yaml:"instance_dir"`  // 实例根目录
	TLS          bool   `yaml:"tls"`
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	CAFile       string `yaml:"ca_file"`

	// CgroupRoot cgroup v2 资源限制的根目录（空则用 /sys/fs/cgroup/atlmcpanel）。
	// 用于给实例施加 CPU 配额；不可用时自动降级为不限制。
	CgroupRoot string `yaml:"cgroup_root"`
	// CgroupEnabled 是否启用 cgroup 资源限制（默认 true，不可用时自动降级）
	CgroupEnabled *bool `yaml:"cgroup_enabled"`

	// ---- frps 管理 API（实例级流量统计）----
	//
	// 配置后，实例页的「网络流量」将使用**该实例隧道**的真实数据；
	// 留空则回退为节点整机的聚合吞吐（无法区分是哪个实例在用带宽）。
	//
	// 需要在 frps 配置中启用 webServer，例如：
	//   webServer.addr = "127.0.0.1"
	//   webServer.port = 7400
	//   webServer.user = "atlmcpanel"
	//   webServer.password = "******"
	FRPAdminAddr     string `yaml:"frp_admin_addr"`     // 如 127.0.0.1:7400
	FRPAdminUser     string `yaml:"frp_admin_user"`     // Basic Auth 用户名
	FRPAdminPassword string `yaml:"frp_admin_password"` // Basic Auth 密码

	// BackupRoot 备份集中存放的根目录（空则放在各实例目录下的 backups/）。
	//
	// 典型用法：把实例放在 SSD 上以获得快速的世界读写，把备份指向大容量
	// 机械盘（如 /mnt/hdd/atlmcpanel-backups）。备份是顺序读写，
	// 对随机 IO 无要求，用机械盘承载可显著降低每 GB 成本，
	// 同时避免备份膨胀挤占实例所在 SSD 的空间。
	// 实际路径为 <backup_root>/<instance_id>/，便于按实例管理与配额。
	BackupRoot string `yaml:"backup_root"`

	// JobWorkers 节点公共任务队列的并发度（压缩 / 解压 / 大目录复制）。
	//
	// 这些操作瓶颈几乎总在磁盘上，并发跑多个只会让机械盘/虚拟盘陷入
	// 随机寻道，总吞吐反而下降，更糟的是把 IO 抢光、让在线玩家卡顿。
	// 因此默认 **1**（串行）；只有在实例目录位于高性能 NVMe 上、
	// 且确实需要吞吐时才建议调到 2~3。
	JobWorkers int `yaml:"job_workers"`

	// ResourceDir 节点共享资源目录（管理员统一上传的 jar 等）。
	//
	// 空则用 <实例根目录>/../resources。与实例目录**分开放**是有意的：
	//   - 实例目录受软配额统计，共享资源放进去会被算进每个实例的用量；
	//   - 实例可被删除/清理，而共享资源不该被实例层的操作波及；
	//   - 大 jar 单独挂盘或单独备份也更方便。
	ResourceDir string `yaml:"resource_dir"`
}

// Defaults 填充 Daemon 默认值。
func (d *DaemonConfig) Defaults() {
	if d.GRPCListen == "" {
		d.GRPCListen = ":9091"
	}
	if d.InstanceDir == "" {
		d.InstanceDir = "instances"
	}
	if d.ResourceDir == "" {
		// 与实例根目录平级，而不是塞在它里面 —— 见 ResourceDir 的注释
		d.ResourceDir = filepath.Join(filepath.Dir(filepath.Clean(d.InstanceDir)), "resources")
	}
}

// DBConfig 数据库配置。
type DBConfig struct {
	Driver string `yaml:"driver"` // sqlite3 / postgres
	DSN    string `yaml:"dsn"`

	// 面板自身数据库的自动备份
	BackupEnabled  *bool  `yaml:"backup_enabled"`  // 指针以便区分「未配置」与「显式关闭」；默认开启
	BackupDir      string `yaml:"backup_dir"`      // 默认 <dsn 所在目录>/backups
	BackupKeep     int    `yaml:"backup_keep"`     // 保留份数，默认 7
	BackupInterval string `yaml:"backup_interval"` // 如 "24h"，默认 24h
}

// BackupEnabledOr 返回是否启用自动备份（未配置时使用 defaultVal）。
func (c *DBConfig) BackupEnabledOr(defaultVal bool) bool {
	if c.BackupEnabled == nil {
		return defaultVal
	}
	return *c.BackupEnabled
}

// BackupIntervalDuration 解析备份间隔，非法或未配置时返回 def。
func (c *DBConfig) BackupIntervalDuration(def time.Duration) time.Duration {
	if c.BackupInterval == "" {
		return def
	}
	d, err := time.ParseDuration(c.BackupInterval)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// AuthConfig 认证配置。
type AuthConfig struct {
	JWTSecret string `yaml:"jwt_secret"`
}

// Load 从指定路径加载 YAML 配置。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults 填充默认值。
func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.GRPCListen == "" {
		c.Server.GRPCListen = ":9090"
	}
	if c.Server.WebDir == "" {
		c.Server.WebDir = "web/dist" // 前端构建产物目录（可由配置覆盖）
	}
	if c.Server.PanelFrpDir == "" {
		c.Server.PanelFrpDir = "data/panel-frp" // 面板自身穿透工作目录
	}
	if c.Server.PKIDir == "" {
		c.Server.PKIDir = "data/pki" // CA 与节点证书目录
	}
	if c.Server.RemoteInstallDir == "" {
		c.Server.RemoteInstallDir = "/opt/mcpanel"
	}
	if c.Server.DaemonServiceName == "" {
		c.Server.DaemonServiceName = "atlmcpanel-daemon"
	}
	if c.Server.DaemonGRPCListen == "" {
		c.Server.DaemonGRPCListen = ":9091"
	}
	if c.DB.Driver == "" {
		c.DB.Driver = "sqlite3"
		c.DB.DSN = "data/mcpanel.db"
	}
	if c.DB.BackupDir == "" {
		c.DB.BackupDir = filepath.Join(filepath.Dir(c.DB.DSN), "backups")
	}
	if c.DB.BackupKeep <= 0 {
		c.DB.BackupKeep = 7
	}
	c.Daemon.Defaults()
}

// MinJWTSecretLength JWT 密钥的最小长度。
const MinJWTSecretLength = 32

// placeholderExact 完全匹配即视为占位密钥。
var placeholderExact = map[string]bool{
	"":                       true,
	"secret":                 true,
	"password":               true,
	"changeme":               true,
	"change_me":              true,
	"jwt_secret":             true,
	"your_secret":            true,
	"your_secret_here":       true,
	"atlmcpanel":             true,
	"test":                   true,
	"dev":                    true,
}

// placeholderFragments 含这些片段即视为示例密钥。
var placeholderFragments = []string{
	"change_me", "changeme", "your_secret", "placeholder", "example", "replace_me", "todo",
}

// IsPlaceholderSecret 判断密钥是否为示例/占位值。
// 注意使用精确匹配而非宽泛子串匹配，避免把 "short-secret" 之类误判为占位符
// （这类值应走长度校验而被明确拒绝）。
func IsPlaceholderSecret(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	if placeholderExact[low] {
		return true
	}
	for _, f := range placeholderFragments {
		if strings.Contains(low, f) {
			return true
		}
	}
	return false
}

// ResolveJWTSecret 解析可用的 JWT 密钥。
//
// 规则：
//   - 已显式配置且强度足够 → 直接使用
//   - 显式配置但过短 → 报错（避免弱密钥导致 token 可被暴力破解）
//   - 未配置或仍为占位符 → 生成随机密钥并持久化到 secretPath（权限 0600），
//     使重启后已签发的会话仍然有效
//
// 返回值第二项表示是否为新生成的密钥（供调用方记录日志）。
func (c *Config) ResolveJWTSecret(secretPath string) (string, bool, error) {
	cfgSecret := strings.TrimSpace(c.Auth.JWTSecret)

	if cfgSecret != "" && !IsPlaceholderSecret(cfgSecret) {
		if len(cfgSecret) < MinJWTSecretLength {
			return "", false, fmt.Errorf(
				"auth.jwt_secret 过短（%d 字符），至少需要 %d 字符；请使用随机密钥",
				len(cfgSecret), MinJWTSecretLength)
		}
		return cfgSecret, false, nil
	}

	// 尝试读取已持久化的自动生成密钥
	if b, err := os.ReadFile(secretPath); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= MinJWTSecretLength {
			c.Auth.JWTSecret = s
			return s, false, nil
		}
	}

	// 生成新密钥并落盘
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("生成 JWT 密钥失败: %w", err)
	}
	secret := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o755); err != nil {
		return "", false, fmt.Errorf("创建密钥目录失败: %w", err)
	}
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		return "", false, fmt.Errorf("写入 JWT 密钥失败: %w", err)
	}
	c.Auth.JWTSecret = secret
	return secret, true, nil
}