// 临时工具：在面板数据库里确保一个已知口令的管理员账号（仅用于 VM 上的端到端验证）。
//
// 为什么不用 SignToken 直接签令牌：那样绕过的是"登录校验"这一环，而我想验的路径
// 恰恰从 HTTP 入口开始（含鉴权中间件）。用真实登录拿到真令牌，覆盖更完整。
//
// 这个工具**只应该出现在开发机上**：验证完就删，不进仓库。
package main

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	cfgPath := "/opt/mcpanel/config.yaml"
	user := "verify"
	pass := "verify-9f3a2b"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		os.Exit(1)
	}
	d, err := db.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer d.Close()

	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成哈希失败:", err)
		os.Exit(1)
	}
	res, err := d.Exec(`
		INSERT INTO users (username, password_hash, role, status) VALUES (?, ?, 'admin', 'active')
		ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash, role = 'admin', status = 'active'`,
		user, string(hash))
	if err != nil {
		fmt.Fprintln(os.Stderr, "写入用户失败:", err)
		os.Exit(1)
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		_ = d.QueryRow(`SELECT id FROM users WHERE username = ?`, user).Scan(&id)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err == nil {
		fmt.Fprintf(os.Stderr, "（库中共 %d 个用户）\n", count)
	}
	_ = sql.ErrNoRows
	fmt.Printf("%s %s %d\n", user, pass, id)
}
