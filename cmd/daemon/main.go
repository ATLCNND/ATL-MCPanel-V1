// dsh-daemon 是 ATL-MCPanel 的节点守护进程入口（部署在每台 MC 实例 VM）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon"
)

func main() {
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
