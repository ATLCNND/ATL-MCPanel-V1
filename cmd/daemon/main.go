// atl-daemon 是 ATL-MCPanel 的节点守护进程入口（部署在每台 MC 实例 VM）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon"
)

func main() {
	// 子命令：`atl-daemon enroll --panel ... --token ...`（凭证交换，M3）
	// 它不是常驻进程，跑一次就退出，所以放在 flag 解析之前单独分派。
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		runEnroll(os.Args[2:])
		return
	}

	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	logLevel := flag.String("log-level", "info", "日志级别: debug/info/warn/error")
	showVersion := flag.Bool("version", false, "显示版本信息并退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	log := logger.New(*logLevel)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	d, err := daemon.New(cfg.Daemon, log)
	if err != nil {
		log.Error("初始化 Daemon 失败", "error", err)
		os.Exit(1)
	}

	log.Info("Daemon 启动", "node_id", cfg.Daemon.NodeID, "panel", cfg.Daemon.PanelAddress)

	// 优雅退出：systemd 停止本单元时发 SIGTERM。
	//
	// 为什么必须处理它（否则会留下孤儿进程）：
	//   - unit 用的是 **KillMode=process**，而这是必须保持的 ——
	//     改成 control-group 会让停 Daemon **连带杀掉用户的 Minecraft 实例**
	//   - 于是 Daemon 一被信号杀掉，它自己的子进程 frpc 就没人回收，
	//     会变成 PPID=1 的孤儿继续跑（2026-09-13 收工实测到过一个）
	//
	// 这里只**请求退出**（让 Run 的心跳循环返回、defer 正常执行），
	// 真正的 frpc 清理放在 Run 返回之后 —— 不用 os.Exit 跳过 defer。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info("收到退出信号，准备优雅退出", "signal", sig.String())
		d.Shutdown()
	}()

	runErr := d.Run()

	// 退出前收走自己拉起的 frpc 子进程（只停 frpc，**不碰 java**）。
	// 正常退出与出错退出都要做，否则一样会留孤儿。
	if n := d.StopFrpAll(); n > 0 {
		log.Info("已停止 frpc 子进程", "count", n)
	}

	if runErr != nil {
		// 收到退出信号导致的返回不算故障 —— 否则 systemctl stop 会在日志里
		// 留下一条 ERROR，看着像服务崩了
		if errors.Is(runErr, daemon.ErrShutdown) {
			log.Info("Daemon 已退出")
			return
		}
		log.Error("Daemon 运行失败", "error", runErr)
		os.Exit(1)
	}
	log.Info("Daemon 已退出")
}

// runEnroll 处理 `enroll` 子命令：向面板换一张节点证书（不需要 SSH，也不需要面板存凭据）。
func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	panel := fs.String("panel", "", "面板地址，例如 http://1.2.3.4:25080（启用安全入口时要带上 /<入口>）")
	token := fs.String("token", "", "面板签发的一次性 token")
	name := fs.String("name", "", "节点显示名（默认用主机名）")
	certDir := fs.String("cert-dir", "", "证书落盘目录（默认取 config 里 daemon.cert_file 的目录）")
	stateDir := fs.String("state-dir", "", "存放 node_uid 的目录（默认取 config 里 daemon.state_dir）")
	// 默认取**二进制旁边**的 config.yaml（安装布局是 <dir>/bin/atl-daemon + <dir>/config.yaml）。
	// 用相对 "config.yaml" 的话，从别的目录跑 enroll 会读不到配置、进而算出另一个 state 目录，
	// 生成新 node_uid → 换证书时被判身份不匹配（实测踩到两次）。
	defCfg := "config.yaml"
	if exe, err := os.Executable(); err == nil {
		if p, err := filepath.Abs(filepath.Join(filepath.Dir(filepath.Dir(exe)), "config.yaml")); err == nil {
			if _, statErr := os.Stat(p); statErr == nil {
				defCfg = p
			}
		}
	}
	cfgPath := fs.String("config", defCfg, "配置文件路径（用于取 node_uid 目录与证书目录的默认值）")
	_ = fs.Parse(args)

	// 默认值取运行中的配置：**必须复用同一个 state 目录**，否则会生成新的 node_uid ——
	// 而 node_uid 是这台机器的身份，用新身份去换证书必然被面板判为「token 与 node_uid 不匹配」
	//（实测踩到：默认 "state" 与实际 state 目录不一致，token 就被拒了）。
	// 基准目录：**配置文件所在目录**（也就是安装目录）。
	// 运行期 Daemon 的 state/certs 默认都在安装目录下，这里必须对齐 —— 用相对路径的话，
	// 换个当前目录跑 enroll 就会生成另一个 node_uid，换证书时被判「身份不匹配」（实测踩到）。
	base := filepath.Dir(*cfgPath)
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	cfgStateDir := filepath.Join(base, "state")
	cfgCertDir := filepath.Join(base, "certs")
	if cfg, err := config.Load(*cfgPath); err == nil {
		if cfg.Daemon.StateDir != "" {
			cfgStateDir = cfg.Daemon.StateDir
			// config.Load 会按 instance_dir 推导 state_dir，而 instance_dir 常写成相对值
			// （安装脚本生成的就是 "instances"），于是推导结果是**相对路径 "state"**。
			// 相对路径取决于当前工作目录 —— 从别处运行 enroll 就会落到另一个目录、生成
			// 新的 node_uid，进而被判「身份不匹配」（实测踩到）。这里统一按配置文件目录补全。
			if !filepath.IsAbs(cfgStateDir) {
				cfgStateDir = filepath.Join(base, cfgStateDir)
			}
		}
		if cfg.Daemon.CertFile != "" {
			cfgCertDir = filepath.Dir(cfg.Daemon.CertFile)
			if !filepath.IsAbs(cfgCertDir) {
				cfgCertDir = filepath.Join(base, cfgCertDir)
			}
		}
	}
	if *stateDir == "" {
		*stateDir = cfgStateDir
	}
	if *certDir == "" {
		*certDir = cfgCertDir
	}

	if *panel == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "用法: atl-daemon enroll --panel <面板地址> --token <一次性 token> [--name 名称]")
		os.Exit(2)
	}
	certPath, caPath, resp, err := daemon.Enroll(daemon.EnrollOptions{
		PanelBase: *panel, Token: *token, NodeName: *name,
		CertDir: *certDir, StateDir: *stateDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "❌ 凭证交换失败："+err.Error())
		os.Exit(1)
	}
	fmt.Println("✅ 凭证交换成功")
	fmt.Println("   证书:", certPath)
	fmt.Println("   CA  :", caPath)
	fmt.Println("   身份:", resp.NodeUID)
	fmt.Println("   面板:", resp.PanelGRPC)
	fmt.Println("   到期:", resp.ExpiresAt)
	if resp.RenewAfter != "" {
		fmt.Println("   建议在该时间前续期:", resp.RenewAfter)
	}
	fmt.Println("下一步：把 config.yaml 的 daemon.tls 设为 true，并让 cert_file/key_file/ca_file 指向上面的文件，然后重启服务")
}
