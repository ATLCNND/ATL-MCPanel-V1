package main

// 面板的命令行管理子命令：**首个管理员的正规创建入口**。
//
// ---------------------------------------------------------------------------
// 为什么必须有这个文件（2026-10-12 安全修复的配套）
// ---------------------------------------------------------------------------
// 在它之前，面板"第一个管理员"是由 `POST /api/users` 匿名自助创建的：users 表
// 为空时，第一个来的人自动成为 admin。这是一条**运维者不知道存在的隐藏入口**
// （前端从未调用过它），后果是只要面板在空库状态下对外可达，扫到 8080 的人
// 一条请求就能拿到管理员。已删除该分支并把路由改回 requireAdmin。
//
// 于是首个管理员需要一条新的、**天然可信**的创建途径：本机命令行 —— 能执行它
// 就意味着已经有 shell 权限，而这个权限本来就等价于面板的完全控制权。这也正是
// 业界主流做法（Pterodactyl `p:user:make --admin=1`、宝塔安装时建号并打印随机
// 密码、1Panel 交互式建号 + `1pctl user-info` 找回）。
//
// 刻意**不**提供"Web 安装向导 + setup token"那条路：Portainer 为了让"首管创建
// 前也能恢复备份"而故意不鉴权 /api/restore，结果未初始化实例可被接管
// （GHSA-x626-fcwx-f5pc, High）。多一个免鉴权端点就多一份长期风险。
//
// 用法：
//
//	atl-panel user create [-config config.yaml] [-username admin] [-password xxx] [-role admin]
//	atl-panel user passwd [-config config.yaml] -username admin [-password xxx]
//	atl-panel user list   [-config config.yaml]
//
// 与面板服务**可以同时运行**：SQLite 走 WAL + busy_timeout=10s（见 db.Open），
// 管理员的插入不会与面板的写操作互相锁死。

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
)

// cliMinPassword 命令行建号/改密的最低长度。
//
// 比 Web 接口的 6 位更严：这条路径专门用来建**第一个管理员**，而首管口令
// 一旦被猜中就是整台面板的失守，没有理由在这里省长度。
const cliMinPassword = 8

// roleAdmin / roleNodeUser / roleUser 与 internal/panel/httpapi/perms.go 里的
// RoleAdmin / RoleNodeUser / RoleUser 必须一致。
//
// 这里没有直接引用那些常量，是因为它们所在的 httpapi 包会把整个 HTTP 层
// （含前端静态托管、frp 管理）拖进 CLI 的依赖图；为一个三值枚举付这个代价不值。
// 代价是这份字面量要跟着那边改 —— 若哪天角色枚举变化，两边一起改。
const (
	roleAdmin    = "admin"
	roleNodeUser = "nodeuser"
	roleUser     = "user"
)

// runUserCmd 处理 `atl-panel user ...`，返回进程退出码。
//
// cfgPath 是**全局** -config 解析出来的值（可能来自子命令之前，例如
// `atl-panel -config X user create`）。各子命令另外还声明自己的 -config，
// 默认取这个值，于是两种写法都成立：
//
//	atl-panel -config /opt/mcpanel/config.yaml user create ...
//	atl-panel user create -config /opt/mcpanel/config.yaml ...
func runUserCmd(cfgPath string, args []string) int {
	if len(args) == 0 {
		userUsage()
		return 2
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		return userCreate(cfgPath, rest)
	case "passwd", "password":
		return userPasswd(cfgPath, rest)
	case "list", "ls":
		return userList(cfgPath, rest)
	case "-h", "--help", "help":
		userUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知的子命令: %s\n\n", sub)
		userUsage()
		return 2
	}
}

func userUsage() {
	fmt.Fprint(os.Stderr, `用法：
  atl-panel user create [-config 配置文件] [-username 用户名] [-password 口令] [-role admin|nodeuser|user]
  atl-panel user passwd [-config 配置文件] -username 用户名 [-password 新口令]
  atl-panel user list   [-config 配置文件]

说明：
  · create 不写 -password 时会随机生成一个 16 位口令并打印出来，请立刻保存。
  · create -if-empty 只在库中**没有任何账号**时才创建（安装脚本用它保证幂等）。
  · 首个管理员必须用 user create 创建。面板自 2026-10-12 起不再提供任何
    免登录的注册入口 —— 此前"users 表为空时第一个匿名请求者自动成为 admin"
    是一条隐藏入口，会让任何能访问面板端口的人抢到管理员。
  · 默认配置文件为 config.yaml，与面板服务的 -config 含义相同。
`)
}

// cliConfigFlag 给子命令挂上统一的 -config（默认继承全局值）。
func cliConfigFlag(fs *flag.FlagSet, fallback string) *string {
	return fs.String("config", fallback, "配置文件路径（也可写在 user 之前作为全局参数）")
}

// openPanelDB 按配置打开面板数据库，并把**相对 DSN** 解析到配置文件所在目录。
//
// 为什么要显式解析：配置里写的是 `dsn: data/mcpanel.db`（相对路径），
// 面板由 systemd 以 WorkingDirectory=安装目录 启动，所以它解析成
// <安装目录>/data/mcpanel.db。而管理员在任意目录下敲这条命令时，相对路径
// 会指向**当前目录** —— 那会"默默地"在别处建一个空库，然后告诉你"没有用户"。
// 这里统一按配置文件所在目录解析，与面板服务的行为对齐。
func openPanelDB(cfgPath string) (*sql.DB, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败（-config %s）: %w", cfgPath, err)
	}
	driver, dsn := cfg.DB.Driver, cfg.DB.DSN
	if driver == "" {
		driver = "sqlite3"
	}
	if driver == "sqlite3" && dsn != "" &&
		!filepath.IsAbs(dsn) && !strings.HasPrefix(dsn, ":memory:") && !strings.HasPrefix(dsn, "file:") {
		dsn = filepath.Join(filepath.Dir(mustAbs(cfgPath)), dsn)
	}
	d, err := db.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	return d, nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// genPassword 生成一个便于人工转录的随机口令。
//
// 字母表刻意去掉易混字符（0/O、1/l/I）：这个口令是要运维者从终端**抄下来**的，
// 抄错一位的症状是"登录说密码错误"，而人往往会先怀疑是自己记错而不是抄错。
func genPassword(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var sb strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		sb.WriteByte(alphabet[idx.Int64()])
	}
	return sb.String(), nil
}

func validCLIRole(role string) bool {
	switch role {
	case roleAdmin, roleNodeUser, roleUser:
		return true
	}
	return false
}

func userCreate(globalCfg string, args []string) int {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	cfgPath := cliConfigFlag(fs, globalCfg)
	username := fs.String("username", "admin", "用户名")
	password := fs.String("password", "", "口令；留空则随机生成并打印")
	role := fs.String("role", roleAdmin, "角色: admin | nodeuser | user")
	ifEmpty := fs.Bool("if-empty", false,
		"仅当库中**没有任何账号**时才创建；已有账号则跳过（供安装脚本幂等调用）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help：flag 包已打印用法
		}
		return 2
	}

	*username = strings.TrimSpace(*username)
	if *username == "" {
		fmt.Fprintln(os.Stderr, "错误：-username 不能为空")
		return 2
	}
	if !validCLIRole(*role) {
		fmt.Fprintf(os.Stderr, "错误：-role 只能是 admin / nodeuser / user，收到 %q\n", *role)
		return 2
	}

	generated := false
	if *password == "" {
		pw, err := genPassword(16)
		if err != nil {
			fmt.Fprintf(os.Stderr, "生成随机口令失败: %v\n", err)
			return 1
		}
		*password = pw
		generated = true
	}
	if len(*password) < cliMinPassword {
		fmt.Fprintf(os.Stderr, "错误：口令至少 %d 位\n", cliMinPassword)
		return 2
	}

	d, err := openPanelDB(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		return 1
	}
	defer d.Close()

	// -if-empty：安装脚本用它在"空库"时才建首个管理员。
	//
	// 为什么必须有这道闸：安装脚本是**幂等**的（文档鼓励重复执行来补齐配置），
	// 如果每次都 create，重复执行要么报"用户已存在"（看起来像失败），要么在
	// 某些实现里顺手重置口令 —— 后者会让运维者手里的口令在毫无察觉的情况下
	// 失效。这里明确"非空库就什么都不做"。
	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
		fmt.Fprintf(os.Stderr, "错误：查询用户表失败: %v\n", err)
		return 1
	}
	if *ifEmpty && total > 0 {
		fmt.Printf("已有 %d 个账号，跳过创建（-if-empty）。\n", total)
		fmt.Println("需要新建或重置口令：atl-panel user create / atl-panel user passwd")
		return 0
	}

	hash, err := auth.HashPassword(*password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：口令加密失败: %v\n", err)
		return 1
	}

	var exists int
	if err := d.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, *username).Scan(&exists); err != nil {
		fmt.Fprintf(os.Stderr, "错误：查询用户失败: %v\n", err)
		return 1
	}
	if exists > 0 {
		fmt.Fprintf(os.Stderr, "错误：用户 %q 已存在。改口令请用：atl-panel user passwd -username %s\n", *username, *username)
		return 1
	}

	if _, err := d.Exec(
		`INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)`,
		*username, hash, *role); err != nil {
		fmt.Fprintf(os.Stderr, "错误：创建用户失败: %v\n", err)
		return 1
	}

	fmt.Println()
	fmt.Println("✅ 已创建账号")
	fmt.Printf("   用户名: %s\n", *username)
	fmt.Printf("   角色  : %s\n", *role)
	if generated {
		fmt.Printf("   口令  : %s   ← 随机生成，请立刻保存（不会再次显示）\n", *password)
	} else {
		fmt.Println("   口令  : （使用你通过 -password 指定的值，此处不重复显示）")
	}
	fmt.Println()
	fmt.Println("   下一步：浏览器打开面板地址登录，并尽快在「账号」页修改口令。")
	return 0
}

func userPasswd(globalCfg string, args []string) int {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	cfgPath := cliConfigFlag(fs, globalCfg)
	username := fs.String("username", "", "用户名（必填）")
	password := fs.String("password", "", "新口令；留空则随机生成并打印")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help：flag 包已打印用法
		}
		return 2
	}
	*username = strings.TrimSpace(*username)
	if *username == "" {
		fmt.Fprintln(os.Stderr, "错误：必须指定 -username")
		return 2
	}

	generated := false
	if *password == "" {
		pw, err := genPassword(16)
		if err != nil {
			fmt.Fprintf(os.Stderr, "生成随机口令失败: %v\n", err)
			return 1
		}
		*password = pw
		generated = true
	}
	if len(*password) < cliMinPassword {
		fmt.Fprintf(os.Stderr, "错误：口令至少 %d 位\n", cliMinPassword)
		return 2
	}

	d, err := openPanelDB(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		return 1
	}
	defer d.Close()

	hash, err := auth.HashPassword(*password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：口令加密失败: %v\n", err)
		return 1
	}

	// token_version +1：与网页端改密（handleChangePassword）同一条规矩。
	// JWT 是自证明的、签发后无法撤回，只能靠世代号让**此前签发的所有令牌**失效
	// （见 db/migrate.go v23 与 httpapi/middleware.go 的 authUser）。
	// 少了这一步，"重置被盗账号口令"在令牌有效期内（24 小时）完全不生效。
	res, err := d.Exec(
		`UPDATE users SET password_hash = ?, token_version = COALESCE(token_version,0) + 1 WHERE username = ?`,
		hash, *username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：更新口令失败: %v\n", err)
		return 1
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fmt.Fprintf(os.Stderr, "错误：用户 %q 不存在\n", *username)
		return 1
	}

	fmt.Println()
	fmt.Printf("✅ 已重置 %s 的口令（该账号此前签发的令牌已全部失效，需重新登录）\n", *username)
	if generated {
		fmt.Printf("   新口令: %s   ← 随机生成，请立刻保存（不会再次显示）\n", *password)
	} else {
		fmt.Println("   新口令: （使用你通过 -password 指定的值，此处不重复显示）")
	}
	return 0
}

func userList(globalCfg string, args []string) int {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	cfgPath := cliConfigFlag(fs, globalCfg)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help：flag 包已打印用法
		}
		return 2
	}

	d, err := openPanelDB(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		return 1
	}
	defer d.Close()

	rows, err := d.Query(`SELECT id, username, role, COALESCE(status,''), created_at FROM users ORDER BY id`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：查询失败: %v\n", err)
		return 1
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t用户名\t角色\t状态\t创建时间")
	n := 0
	for rows.Next() {
		var id int64
		var username, role, status, created string
		if err := rows.Scan(&id, &username, &role, &status, &created); err != nil {
			fmt.Fprintf(os.Stderr, "错误：读取行失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", id, username, role, status, created)
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "错误：遍历结果失败: %v\n", err)
		return 1
	}
	_ = w.Flush()

	if n == 0 {
		fmt.Println()
		fmt.Println("（没有任何账号）")
		fmt.Println("首个管理员请用：atl-panel user create -username admin")
	}
	return 0
}
