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

// 迁移版本必须"连续递增"，但**允许有明确记录在案的跳号**。
//
// 为什么不能死守"必须一个一个接"：0.12.x（已撤回）的 `instance_template` 占了 34，
// 而这个产品的库可能经历过 0.12.x —— 34 已经被写进 `schema_migrations`，
// 再用 34 写新迁移会被**静默跳过**（表现为"列没建出来"）。所以只能跳号用 35、36。
//
// 这条用例的价值正是"发现意外的跳号"：真正的冲突（两个迁移抢同一个号）会表现为
// "某个号重复两次"或"跳了一个没人报备的号"，两种都必须红。
// 想新增跳号 → 先在 `skippedVersions` 里写清理由，别偷偷跳。
var skippedVersions = map[int]string{
	34: "被已撤回的 0.12.x 的 instance_template 占用；任何经历过 0.12.x 的库 34 都已存在，复用会被静默跳过",
}

func TestMigrationVersionsAreSequential(t *testing.T) {
	list := AllMigrations()
	if len(list) == 0 {
		t.Fatal("迁移列表不应为空")
	}
	seen := map[int]bool{}
	next := 1
	for _, m := range list {
		if m.Name == "" {
			t.Errorf("迁移 %d 缺少名称", m.Version)
		}
		if seen[m.Version] {
			t.Errorf("迁移版本 %d 出现了两次（这才是真正的冲突）", m.Version)
			continue
		}
		seen[m.Version] = true
		// 允许跳过"报备过的号"
		for next < m.Version {
			if _, ok := skippedVersions[next]; !ok {
				t.Errorf("迁移版本出现未报备的跳号：%d 缺失（若确实要跳，请写进 skippedVersions 并说明理由）", next)
			}
			next++
		}
		if m.Version != next {
			t.Errorf("迁移版本乱序或重复：得到 %d，期望 %d", m.Version, next)
		}
		next++
	}
	// 报备表里的号不应该后来又被用上
	for v := range skippedVersions {
		if seen[v] {
			t.Errorf("迁移版本 %d 在 skippedVersions 里标为跳号，却真的存在一条迁移", v)
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
