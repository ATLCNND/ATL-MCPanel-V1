// Package db 封装数据库连接、迁移与连接池调优。
package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite 驱动（CGO，需 gcc）
)

// Open 打开数据库并应用迁移。
func Open(driver, dsn string) (*sql.DB, error) {
	// SQLite 并发调优：WAL 允许读写并行，busy_timeout 让并发写等待而非直接报错。
	// 若不设置，并发请求（前端轮询 + 用户操作 + 审计写入）会间歇性触发
	// "database is locked"，表现为列表接口偶发 500。
	if driver == "sqlite3" {
		dsn = normalizeSQLiteDSN(dsn)
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
