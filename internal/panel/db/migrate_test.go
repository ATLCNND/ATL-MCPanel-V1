package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// expectedTables 迁移后应存在的表。
var expectedTables = []string{
	"users", "nodes", "instances", "instance_assignments",
	"frps_servers", "tunnels", "panel_tunnel", "audit_logs",
	"schema_migrations",
}

func TestMigrationsOnFreshDatabase(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fresh.db")

	d, err := Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer d.Close()

	for _, tbl := range expectedTables {
		var name string
		err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("表 %s 不存在: %v", tbl, err)
		}
	}

	v, err := CurrentVersion(d)
	if err != nil {
		t.Fatalf("读取版本失败: %v", err)
	}
	if v != LatestVersion() {
		t.Errorf("版本应已应用到 %d，实际 %d", LatestVersion(), v)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "twice.db")

	d1, err := Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	// 写入一条数据，验证二次迁移不会破坏数据
	if _, err := d1.Exec(`INSERT INTO users (username, password_hash, role) VALUES ('tester','x','user')`); err != nil {
		t.Fatalf("插入测试数据失败: %v", err)
	}
	d1.Close()

	// 再次打开（重复执行迁移）不应报错
	d2, err := Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("重复迁移失败（应幂等）: %v", err)
	}
	defer d2.Close()

	var count int
	if err := d2.QueryRow(`SELECT COUNT(*) FROM users WHERE username='tester'`).Scan(&count); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if count != 1 {
		t.Errorf("重复迁移后数据应保留，实际 count=%d", count)
	}

	v, _ := CurrentVersion(d2)
	if v != LatestVersion() {
		t.Errorf("版本应为 %d，实际 %d", LatestVersion(), v)
	}
}

func TestMigrationVersionsAreSequential(t *testing.T) {
	list := AllMigrations()
	if len(list) == 0 {
		t.Fatal("迁移列表不应为空")
	}
	for i, m := range list {
		want := i + 1
		if m.Version != want {
			t.Errorf("迁移版本必须从 1 连续递增：第 %d 项版本为 %d，期望 %d", i, m.Version, want)
		}
		if m.Name == "" {
			t.Errorf("迁移 %d 缺少名称", m.Version)
		}
	}
}

func TestWALModeEnabled(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "wal.db")
	d, err := Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer d.Close()

	var mode string
	if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if mode != "wal" {
		t.Errorf("应启用 WAL 模式，实际 %q", mode)
	}
}

func TestNormalizeSQLiteDSN(t *testing.T) {
	got := normalizeSQLiteDSN("data/mcpanel.db")
	for _, want := range []string{"_journal_mode=WAL", "_busy_timeout=10000", "_foreign_keys=on"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN 应包含 %s，实际 %s", want, got)
		}
	}
	// 已显式配置时不应覆盖
	got2 := normalizeSQLiteDSN("x.db?_journal_mode=DELETE")
	if strings.Contains(got2, "_journal_mode=WAL") {
		t.Errorf("显式配置不应被覆盖，实际 %s", got2)
	}
	// 已有 query 时应使用 & 连接
	if !strings.Contains(normalizeSQLiteDSN("x.db?a=1"), "?a=1&_journal_mode=WAL") {
		t.Errorf("已有 query 参数时应使用 & 连接，实际 %s", normalizeSQLiteDSN("x.db?a=1"))
	}
}
