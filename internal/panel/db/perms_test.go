package db

import (
	"os"
	"path/filepath"
	"testing"
)

// dbPermCases 覆盖 DSN 的几种真实形态，确保从里面提取路径的逻辑是对的。
func TestRestrictSQLiteFilePerms(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "mcpanel.db")

	// 造出三个文件（模拟 WAL 模式下的真实情况）并把权限放开到 644，
	// 也就是 SQLite 用默认 umask 建出来的那个权限
	files := []string{base, base + "-wal", base + "-shm"}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// DSN 带查询参数（真实配置就是这种）
	restrictSQLiteFilePerms(base + "?_journal_mode=WAL&_busy_timeout=10000")

	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatalf("%s 应存在: %v", filepath.Base(f), err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s 权限应为 0600，实际 %04o", filepath.Base(f), fi.Mode().Perm())
		}
	}
}

// 不带查询参数的 DSN 也要能处理。
func TestRestrictSQLiteFilePermsNoQuery(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "plain.db")
	if err := os.WriteFile(base, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	restrictSQLiteFilePerms(base)
	fi, _ := os.Stat(base)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，实际 %04o", fi.Mode().Perm())
	}
}

// 文件不存在时不该 panic、不该报错（首次启动、或还没写过 WAL 时就是这样）。
func TestRestrictSQLiteFilePermsMissingFiles(t *testing.T) {
	restrictSQLiteFilePerms(filepath.Join(t.TempDir(), "nope.db"))
	restrictSQLiteFilePerms(filepath.Join(t.TempDir(), "nope.db") + "?_journal_mode=WAL")
}

// 内存库不该被当成文件路径处理。
func TestRestrictSQLiteFilePermsMemoryDSN(t *testing.T) {
	restrictSQLiteFilePerms(":memory:")
	restrictSQLiteFilePerms("file::memory:?cache=shared")
	restrictSQLiteFilePerms("")
}
