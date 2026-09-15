package dbbackup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
)

func newTestDB(t *testing.T) (dbPath string, closeFn func()) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "panel.db")
	d, err := db.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO users (username, password_hash, role) VALUES ('tester','x','admin')`); err != nil {
		t.Fatalf("写入测试数据失败: %v", err)
	}
	return dbPath, func() { d.Close() }
}

func TestBackupCreatesConsistentSnapshot(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()

	d, err := db.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	dir := t.TempDir()
	m := New(d, Options{Dir: dir})

	path, err := m.Backup()
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("备份文件不应为空")
	}

	// 备份应可独立打开并读到数据（验证快照完整性）
	bd, err := db.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("备份文件无法打开: %v", err)
	}
	defer bd.Close()
	var count int
	if err := bd.QueryRow(`SELECT COUNT(*) FROM users WHERE username='tester'`).Scan(&count); err != nil {
		t.Fatalf("查询备份失败: %v", err)
	}
	if count != 1 {
		t.Errorf("备份应包含数据，实际 count=%d", count)
	}
}

func TestBackupPruneKeepsLimit(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()
	d, _ := db.Open("sqlite3", dbPath)
	defer d.Close()

	dir := t.TempDir()
	m := New(d, Options{Dir: dir, Keep: 3})

	// 连续备份 5 次
	for i := 0; i < 5; i++ {
		if _, err := m.Backup(); err != nil {
			t.Fatalf("第 %d 次备份失败: %v", i+1, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("应保留 3 份备份，实际 %d", len(list))
	}
	// 结果应按时间倒序
	if len(list) >= 2 && list[0].ModTime.Before(list[1].ModTime) {
		t.Error("备份列表应按时间倒序")
	}
}

func TestListEmptyDir(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()
	d, _ := db.Open("sqlite3", dbPath)
	defer d.Close()

	m := New(d, Options{Dir: filepath.Join(t.TempDir(), "not-created")})
	list, err := m.List()
	if err != nil {
		t.Fatalf("目录不存在时不应报错: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("应为空列表，实际 %d", len(list))
	}
}

func TestListIgnoresUnrelatedFiles(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()
	d, _ := db.Open("sqlite3", dbPath)
	defer d.Close()

	dir := t.TempDir()
	m := New(d, Options{Dir: dir})
	if _, err := m.Backup(); err != nil {
		t.Fatal(err)
	}
	// 干扰文件
	_ = os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "other.db"), []byte("x"), 0o644)

	list, _ := m.List()
	if len(list) != 1 {
		t.Errorf("应仅识别备份文件，实际 %d 个", len(list))
	}
}

func TestNeedBackupLogic(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()
	d, _ := db.Open("sqlite3", dbPath)
	defer d.Close()

	dir := t.TempDir()
	// 间隔设为 1 小时
	m := New(d, Options{Dir: dir, Interval: time.Hour})

	// 无备份时应需要备份
	if !m.needBackup() {
		t.Error("无备份时应需要备份")
	}
	if _, err := m.Backup(); err != nil {
		t.Fatal(err)
	}
	// 刚备份完不需要
	if m.needBackup() {
		t.Error("刚完成备份后不应立即再备份")
	}

	// 间隔设为 0（默认 24h）也不应立刻需要
	m2 := New(d, Options{Dir: dir})
	if m2.needBackup() {
		t.Error("存在新近备份时不应需要备份")
	}

	// 把备份文件时间改老
	list, _ := m.List()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(list[0].Path, old, old); err != nil {
		t.Fatal(err)
	}
	if !m.needBackup() {
		t.Error("备份过期后应需要备份")
	}
}

func TestBackupDefaults(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()
	d, _ := db.Open("sqlite3", dbPath)
	defer d.Close()

	m := New(d, Options{Dir: t.TempDir()})
	if m.Keep() != DefaultKeep {
		t.Errorf("默认保留份数应为 %d，实际 %d", DefaultKeep, m.Keep())
	}
}

func TestStartStop(t *testing.T) {
	dbPath, closeFn := newTestDB(t)
	defer closeFn()
	d, _ := db.Open("sqlite3", dbPath)
	defer d.Close()

	m := New(d, Options{Dir: t.TempDir(), Interval: 20 * time.Millisecond})
	m.Start()
	// 启动时因无备份，应立即备份一次
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if list, _ := m.List(); len(list) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if list, _ := m.List(); len(list) == 0 {
		t.Error("Start 后应自动完成首次备份")
	}
	m.Stop()
	m.Stop() // 重复停止不应 panic
}
