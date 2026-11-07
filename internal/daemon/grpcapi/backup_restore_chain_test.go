package grpcapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 链式恢复（增量第 3 步）。
//
// 这一层的错误全都是「看起来成功」型的：少了文件、删错文件、或者拿一个中间态
// 覆盖掉还能用的实例。所以用例一律**先把实例目录弄坏**（改内容、造脏文件、删新文件），
// 再用 Restore 去救，然后逐文件核对最终内容 —— 不看返回值，看磁盘。

// wipe 把实例目录里的业务文件清掉/弄脏（保留 backups/，那是账本本身）。
func wipe(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == backupsDirName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

func mustRestore(t *testing.T, e *backupTestEnv, backupID string) *pb.OperationResponse {
	t.Helper()
	resp, err := e.srv.Restore(context.Background(), &pb.RestoreRequest{
		InstanceId: e.instID,
		BackupId:   backupID,
	})
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	return resp
}

func readRestored(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(b)
}

// 改一个 / 删一个 / 增一个 → 点最后一环的 inc 恢复，三种情况都要对。
func TestRestoreChainAppliesBaseIncsAndTombstones(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/keep.dat", "K1")
	e.write(t, "world/mod.dat", "M1")
	e.write(t, "world/del.dat", "D1")
	e.write(t, "server.properties", "motd=hi\n")

	if _, m := e.backup(t, autoBackupName, true); m.Kind != "base" {
		t.Fatalf("首次自动备份应为基线，实际 %q", m.Kind)
	}

	// 第二环：改一个、删一个、增一个
	e.write(t, "world/mod.dat", "M2-longer")
	if err := os.Remove(filepath.Join(e.dir, "world", "del.dat")); err != nil {
		t.Fatal(err)
	}
	e.write(t, "world/new.dat", "N1")
	incName, inc := e.backup(t, autoBackupName, true)
	if inc.Kind != "inc" {
		t.Fatalf("第二环应为增量，实际 %q", inc.Kind)
	}
	if len(inc.Deleted) != 1 || inc.Deleted[0] != "world/del.dat" {
		t.Fatalf("墓碑应为 [world/del.dat]，实际 %v", inc.Deleted)
	}

	// 灾难现场：内容被改脏、旧文件又冒出来、新增的没了
	wipe(t, e.dir)
	e.write(t, "world/mod.dat", "GARBAGE")
	e.write(t, "world/del.dat", "STALE")
	e.write(t, "world/keep.dat", "GARBAGE-TOO")

	resp := mustRestore(t, e, incName)
	if !resp.Success {
		t.Fatalf("链式回滚失败：%s", resp.Error)
	}
	if !strings.Contains(resp.Message, "链 2 环") {
		t.Errorf("回滚结果应说明链的环数，实际 %q", resp.Message)
	}

	if got := readRestored(t, e.dir, "world/mod.dat"); got != "M2-longer" {
		t.Errorf("改过的文件没恢复成第二环的内容，实际 %q", got)
	}
	if got := readRestored(t, e.dir, "world/keep.dat"); got != "K1" {
		t.Errorf("第一环的文件内容不对，实际 %q", got)
	}
	if got := readRestored(t, e.dir, "world/new.dat"); got != "N1" {
		t.Errorf("第二环新增的文件没恢复，实际 %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "world", "del.dat")); !os.IsNotExist(err) {
		t.Error("墓碑文件应被删除，实际还在")
	}
	if got := readRestored(t, e.dir, "server.properties"); got != "motd=hi\n" {
		t.Errorf("配置文件没恢复，实际 %q", got)
	}
}

// 墓碑→再加回来：链上删过、后面的环又加回来的文件，最终必须存在。
// （这正是「先全部覆盖再统一删墓碑」会做错的形状。）
func TestRestoreChainHonoursReAddedFile(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/a.dat", "A1")
	if _, m := e.backup(t, autoBackupName, true); m.Kind != "base" {
		t.Fatalf("首次应为基线，实际 %q", m.Kind)
	}

	// 第二环：删掉 a.dat
	if err := os.Remove(filepath.Join(e.dir, "world", "a.dat")); err != nil {
		t.Fatal(err)
	}
	if _, m := e.backup(t, autoBackupName, true); len(m.Deleted) != 1 {
		t.Fatalf("第二环应记一条墓碑，实际 %v", m.Deleted)
	}

	// 第三环：又加回来（内容不同）
	e.write(t, "world/a.dat", "A2-back")
	third, m3 := e.backup(t, autoBackupName, true)
	if m3.Kind != "inc" || len(m3.Deleted) != 0 {
		t.Fatalf("第三环应是无墓碑的增量，实际 kind=%q deleted=%v", m3.Kind, m3.Deleted)
	}

	wipe(t, e.dir)
	resp := mustRestore(t, e, third)
	if !resp.Success {
		t.Fatalf("链式回滚失败：%s", resp.Error)
	}
	if got := readRestored(t, e.dir, "world/a.dat"); got != "A2-back" {
		t.Errorf("删掉后又加回来的文件最终应存在且为最后内容，实际 %q", got)
	}
}

// 链上任何一个包坏了，都要**在动实例目录之前**拒绝 —— 这是先解到暂存目录的全部意义。
func TestRestoreChainRejectsCorruptPackageBeforeTouchingInstance(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL-v1")
	baseName, _ := e.backup(t, autoBackupName, true)
	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	incName, _ := e.backup(t, autoBackupName, true)

	// 把基线包改坏一个字节（大小不变，改内容 → 整包 sha256 对不上）
	p := filepath.Join(e.bdir, baseName)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// 实例里留一份"还能用"的数据，回滚失败后必须原样还在
	wipe(t, e.dir)
	e.write(t, "world/level.dat", "ORIGINAL")

	resp := mustRestore(t, e, incName)
	if resp.Success {
		t.Fatal("链上有包校验不过，回滚本应失败")
	}
	if !strings.Contains(resp.Error, baseName) {
		t.Errorf("错误信息应指出是哪个包坏了（%s），实际 %q", baseName, resp.Error)
	}
	if got := readRestored(t, e.dir, "world/level.dat"); got != "ORIGINAL" {
		t.Errorf("回滚失败后实例目录不该被动过，实际内容 %q", got)
	}
	// 暂存目录也要清干净（别把几 GB 的中间产物留在节点上）
	parent := filepath.Dir(e.dir)
	leftovers, _ := filepath.Glob(filepath.Join(parent, ".restore-*"))
	if len(leftovers) != 0 {
		t.Errorf("暂存目录没有清理：%v", leftovers)
	}
}

// 链缺环（基线被删了/挪走了）必须明确失败，不能"能恢复多少算多少"。
func TestRestoreChainRefusesMissingBase(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL-v1")
	baseName, _ := e.backup(t, autoBackupName, true)
	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	incName, _ := e.backup(t, autoBackupName, true)

	if err := os.Remove(filepath.Join(e.bdir, baseName)); err != nil {
		t.Fatal(err)
	}
	resp := mustRestore(t, e, incName)
	if resp.Success {
		t.Fatal("基线包不在时不该恢复成功")
	}
	if !strings.Contains(resp.Error, "备份链不完整") {
		t.Errorf("错误信息应说明链不完整，实际 %q", resp.Error)
	}
}

// 手动备份（不在链上）仍然走单包恢复：行为与既有版本完全一致。
func TestRestoreManualBackupStaysSinglePackage(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL-v1")
	e.write(t, "server.properties", "motd=hi\n")
	if _, m := e.backup(t, autoBackupName, true); m.Kind != "base" {
		t.Fatalf("首次应为基线，实际 %q", m.Kind)
	}
	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	// 手动备份 = 完整快照，且不入链
	manualName, manual := e.backup(t, "手动-20261107", true)
	if manual.Kind != "base" {
		t.Fatalf("手动备份应是完整快照，实际 %q", manual.Kind)
	}

	wipe(t, e.dir)
	resp := mustRestore(t, e, manualName)
	if !resp.Success {
		t.Fatalf("手动备份回滚失败：%s", resp.Error)
	}
	if strings.Contains(resp.Message, "链") {
		t.Errorf("手动备份不该被当成一条链来恢复，实际 %q", resp.Message)
	}
	if got := readRestored(t, e.dir, "world/level.dat"); got != "LEVEL-v2-longer" {
		t.Errorf("手动备份里的内容没恢复，实际 %q", got)
	}
}

// prev_id 成环时必须停下来（清单是磁盘上的普通文件，可能被手工改过）。
func TestRestoreChainStopsOnCycle(t *testing.T) {
	e := newBackupTestEnv(t)
	if err := os.MkdirAll(e.bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, prev string) {
		if err := os.WriteFile(filepath.Join(e.bdir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := backupManifest{Schema: 1, Kind: "inc", PrevID: prev}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.bdir, name+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bk_1_auto.tar.gz", "bk_2_auto.tar.gz")
	write("bk_2_auto.tar.gz", "bk_1_auto.tar.gz")

	if _, err := e.srv.restoreChainOf(e.bdir, filepath.Join(e.bdir, "bk_1_auto.tar.gz")); err == nil {
		t.Fatal("成环的链不该被接受")
	} else if !strings.Contains(err.Error(), "环") {
		t.Errorf("错误信息应说明成环，实际 %v", err)
	}
}
