// Package db 封装数据库连接、迁移与连接池调优。
package db

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite 驱动（CGO，需 gcc）
)

// restrictSQLiteFilePerms 把 SQLite 的数据文件收紧到 0600。
//
// ---------------------------------------------------------------------------
// 为什么必须做（2026-09-17 在公网机上实测）
// ---------------------------------------------------------------------------
// SQLite 建库用的是**进程 umask**（默认 022），于是 mcpanel.db 出来是 0644 ——
// 同一台机器上**任何用户都能读**。实测 `sudo -u nobody test -r mcpanel.db`
// 是**通过**的，`-wal` 也一样。
//
// 而这个库里不只存 bcrypt 口令哈希（那个泄露了还难破解），还有：
//   - 节点 SSH 凭据（`nodes.ssh_auth`，目前是明文入库）
//   - 各条线路的 frps token（拿到就能往 frps 上开任意端口）
//
// ⚠️ 只 chmod 主库文件是不够的：WAL 模式下**正在写入的数据在 `-wal` 里**，
// 实测那台机器上 `-wal` 有 2.7MB，比主库文件大三个数量级。
// 三个文件必须一起收紧。
func restrictSQLiteFilePerms(dsn string) {
	path := dsn
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		// 文件可能还不存在（例如本次运行还没写过），失败是正常的
		_ = os.Chmod(path+suffix, 0o600)
	}
}

// Open 打开数据库并应用迁移。
func Open(driver, dsn string) (*sql.DB, error) {
	// SQLite 并发调优：WAL 允许读写并行，busy_timeout 让并发写等待而非直接报错。
	// 若不设置，并发请求（前端轮询 + 用户操作 + 审计写入）会间歇性触发
	// "database is locked"，表现为列表接口偶发 500。
	if driver == "sqlite3" {
		dsn = normalizeSQLiteDSN(dsn)
		// 放在 defer 里：此时 -wal/-shm 可能还没创建，要等迁移写完才有；
		// defer 在 Open 返回前执行，那时三个文件都齐了。
		// 出错返回时也会跑一次 —— 库文件可能已经建出来了，同样要收紧。
		defer restrictSQLiteFilePerms(dsn)
	}

	d, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	if driver == "sqlite3" {
		// SQLite 单写入者模型：限制连接数避免写冲突，配合 WAL 保证读不阻塞。
		d.SetMaxOpenConns(4)
		d.SetMaxIdleConns(4)
		d.SetConnMaxLifetime(time.Hour)
	}

	if err := d.Ping(); err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if err := migrate(d); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}
	return d, nil
}

// normalizeSQLiteDSN 为 SQLite DSN 追加并发相关参数（已存在则不覆盖）。
func normalizeSQLiteDSN(dsn string) string {
	params := []string{"_journal_mode=WAL", "_busy_timeout=10000", "_foreign_keys=on"}
	hasQuery := strings.Contains(dsn, "?")
	for _, p := range params {
		key := strings.SplitN(p, "=", 2)[0]
		if strings.Contains(dsn, key+"=") {
			continue // 已显式配置，尊重调用方
		}
		sep := "&"
		if !hasQuery {
			sep = "?"
			hasQuery = true
		}
		dsn += sep + p
	}
	return dsn
}
