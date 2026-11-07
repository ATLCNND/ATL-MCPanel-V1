package grpcapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 增量备份接进主流程（第 2d 步）的用例。
//
// 这一层的错误都是**静默**的：备份看起来成功、清单看起来正常，直到真要恢复时
// 才发现少文件、或者链断了。所以这里不看"函数返回什么"，而是看**包里的内容**
// 与**清单里记的状态**是否符合三条不变式（交接文档 §5.2）：
//  1. 清单记录本轮完整状态，不是"这次打包了什么"；
//  2. 链只认自动备份（手动备份不入链）；
//  3. 环数 ≥ 6 或基线超 7 天 → 建新基线。
//
// ⚠️ 判据只有秒级 mtime + size，所以用例里"改文件"一律顺便改**尺寸**：
// 同一秒内同尺寸的两次写，判据按设计（DESIGN §3.2）判为"没变"。

// backupTestEnv 一个已注册实例 + 它的备份目录。
type backupTestEnv struct {
	srv    *Server
	reg    *registry.Registry
	instID string
	dir    string // 实例目录
	bdir   string // 备份目录（实例目录下的 backups/）
}

func newBackupTestEnv(t *testing.T) *backupTestEnv {
	t.Helper()
	srv, reg := newSecurityTestServer(t, "")
	inst, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	return &backupTestEnv{
		srv:    srv,
		reg:    reg,
		instID: inst.ID,
		dir:    inst.Dir,
		bdir:   filepath.Join(inst.Dir, backupsDirName),
	}
}

// write 在实例目录里写一个文件（rel 用 / 分隔）。
func (e *backupTestEnv) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(e.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backup 跑一次备份，返回包名与清单。
func (e *backupTestEnv) backup(t *testing.T, name string, includeConfig bool) (string, backupManifest) {
	t.Helper()
	resp, err := e.srv.Backup(context.Background(), &pb.BackupRequest{
		InstanceId:    e.instID,
		Name:          name,
		IncludeConfig: includeConfig,
	})
	if err != nil {
		t.Fatalf("Backup 返回错误: %v", err)
	}
	if !resp.Success {
		t.Fatalf("备份失败: %s", resp.Message)
	}
	return resp.BackupId, readChainManifest(t, e.bdir, resp.BackupId)
}

// readChainManifest 读某个备份旁边的清单。
func readChainManifest(t *testing.T, bdir, backupID string) backupManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bdir, backupID+".json"))
	if err != nil {
		t.Fatalf("读清单 %s 失败: %v", backupID, err)
	}
	var m backupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("清单 %s 不是合法 JSON: %v", backupID, err)
	}
	return m
}

// tarFiles 返回包里**普通文件**的内容（目录头不算）。
func tarFiles(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开包 %s 失败: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("包 %s 无法解开: %v", path, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读包 %s 失败: %v", path, err)
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读包 %s 里的 %s 失败: %v", path, hdr.Name, err)
		}
		out[hdr.Name] = string(b)
	}
	return out
}

// filePaths 清单里的路径（排序后便于比较）。
func filePaths(files []manifestFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

// §5.3 第 1 条：改一个文件 → 自动备份是增量，包里只有那个文件，而且明显更小。
func TestAutoBackupIsIncrementalAfterFirstBase(t *testing.T) {
	e := newBackupTestEnv(t)
	// 三份"世界数据"，其中一份较大，方便比较体积
	e.write(t, "world/region/r.0.0.mca", string(make([]byte, 64*1024)))
	e.write(t, "world/level.dat", "LEVEL-v1")
	e.write(t, "server.properties", "motd=hi\n")

	baseName, base := e.backup(t, autoBackupName, true)
	if base.Kind != "base" {
		t.Fatalf("首次自动备份必须是基线，实际 kind=%q", base.Kind)
	}
	if base.PrevID != "" {
		t.Errorf("基线的 prev_id 必须为空（链到此为止），实际 %q", base.PrevID)
	}
	if base.BaseID != baseName {
		t.Errorf("基线的 base_id 应是它自己 %q，实际 %q", baseName, base.BaseID)
	}
	if got := len(tarFiles(t, filepath.Join(e.bdir, baseName))); got != 3 {
		t.Errorf("基线包应含 3 个文件，实际 %d 个", got)
	}

	// 改一个文件（尺寸也变，避免同秒同尺寸判据失效）
	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	incName, inc := e.backup(t, autoBackupName, true)

	if inc.Kind != "inc" {
		t.Fatalf("第二次自动备份应是增量，实际 kind=%q（why=%q）", inc.Kind, "")
	}
	if inc.PrevID != baseName {
		t.Errorf("inc 的 prev_id 应指向上一环 %q，实际 %q", baseName, inc.PrevID)
	}
	if inc.BaseID != baseName {
		t.Errorf("inc 的 base_id 应是 %q，实际 %q", baseName, inc.BaseID)
	}

	// 包里只该有那个改过的文件
	got := tarFiles(t, filepath.Join(e.bdir, incName))
	if len(got) != 1 {
		t.Errorf("增量包应只含 1 个变化文件，实际 %d 个：%v", len(got), keysOf(got))
	}
	if got["world/level.dat"] != "LEVEL-v2-longer" {
		t.Errorf("增量包里的 world/level.dat 内容不对：%q", got["world/level.dat"])
	}

	// 而且明显更小（验收第 1 条的"明显更小"）
	bBase, _ := os.Stat(filepath.Join(e.bdir, baseName))
	bInc, _ := os.Stat(filepath.Join(e.bdir, incName))
	if bBase != nil && bInc != nil && bInc.Size() >= bBase.Size() {
		t.Errorf("增量包应明显小于基线包：inc=%d base=%d", bInc.Size(), bBase.Size())
	}

	// 不变式 1：清单记的是**本轮完整状态**，不是"这次打包了什么"
	if fp := filePaths(inc.Files); len(fp) != 3 {
		t.Errorf("清单必须记录完整状态（3 个文件），实际 %d 个：%v", len(fp), fp)
	}
}

// §5.3 第 2 条：删一个文件 → 清单的 deleted 里有它（墓碑），包里当然没有。
func TestAutoBackupRecordsTombstoneForDeletedFile(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/region/r.0.0.mca", "R00")
	e.write(t, "world/region/r.1.1.mca", "R11")
	e.write(t, "world/level.dat", "LEVEL")

	if _, m := e.backup(t, autoBackupName, true); m.Kind != "base" {
		t.Fatalf("首次自动备份应为基线，实际 %q", m.Kind)
	}

	if err := os.Remove(filepath.Join(e.dir, "world", "region", "r.1.1.mca")); err != nil {
		t.Fatal(err)
	}
	incName, inc := e.backup(t, autoBackupName, true)

	if inc.Kind != "inc" {
		t.Fatalf("第二次自动备份应为增量，实际 %q", inc.Kind)
	}
	if len(inc.Deleted) != 1 || inc.Deleted[0] != "world/region/r.1.1.mca" {
		t.Fatalf("deleted 应恰好是 [world/region/r.1.1.mca]，实际 %v", inc.Deleted)
	}
	if _, ok := tarFiles(t, filepath.Join(e.bdir, incName))["world/region/r.1.1.mca"]; ok {
		t.Error("已删除的文件不该出现在增量包里")
	}
	// 清单的完整状态里也不能再有它（否则下一轮又会被当成"还在"）
	for _, p := range filePaths(inc.Files) {
		if p == "world/region/r.1.1.mca" {
			t.Error("清单的 files 里不该再有已删除的文件")
		}
	}
}

// §5.3 第 3 条：环数到 6 → 下一次自动备份写新基线，不再往上接。
func TestAutoBackupWritesNewBaseAtChainLimit(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL")

	var kinds []string
	var names []string
	for i := 0; i < maxChainDepth+1; i++ {
		name, m := e.backup(t, autoBackupName, true)
		kinds = append(kinds, m.Kind)
		names = append(names, name)
	}

	// 第 1 次是基线，第 2..maxChainDepth 次是增量（环数 2..6），第 7 次必须重新做基线
	want := make([]string, 0, len(kinds))
	want = append(want, "base")
	for i := 1; i < maxChainDepth; i++ {
		want = append(want, "inc")
	}
	want = append(want, "base")
	if len(kinds) != len(want) {
		t.Fatalf("用例自身有问题：kinds=%v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("第 %d 次备份的 kind 应为 %q，实际 %q（全部 kinds=%v）", i+1, want[i], kinds[i], kinds)
		}
	}

	last := readChainManifest(t, e.bdir, names[len(names)-1])
	if last.PrevID != "" {
		t.Errorf("新基线的 prev_id 必须为空，实际 %q", last.PrevID)
	}
	if last.BaseID != names[len(names)-1] {
		t.Errorf("新基线的 base_id 应是它自己，实际 %q", last.BaseID)
	}
	// 新基线必须是**完整**包：链断了以后它得能独立恢复
	if got := len(tarFiles(t, filepath.Join(e.bdir, names[len(names)-1]))); got != 1 {
		t.Errorf("新基线包应含全部 1 个文件，实际 %d 个", got)
	}
}

// 不变式 2：手动备份永远是完整快照，不进入增量链 —— 也不能被当成链尾。
func TestManualBackupStaysFullAndOutOfChain(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL")
	e.write(t, "server.properties", "motd=hi\n")

	autoBase, _ := e.backup(t, autoBackupName, true)

	// 手动备份，而且名字里带 auto —— 面板允许这种名字（"自动化前测试"），
	// 名字判断若用前缀/包含匹配，这份手动备份会被当成链的一部分。
	// 面板的手动路径固定传 include_config=true（手动 = 完整快照），这里照抄。
	manualName, manual := e.backup(t, "自动化前测试", true)
	if manual.Kind != "base" {
		t.Errorf("手动备份永远是完整快照（kind=base），实际 %q", manual.Kind)
	}
	if len(tarFiles(t, filepath.Join(e.bdir, manualName))) != 2 {
		t.Error("手动备份必须包含全部文件，不能是增量")
	}

	// 手动备份之后再来的自动备份，上一环仍应是那条自动链的链尾
	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	_, inc := e.backup(t, autoBackupName, true)
	if inc.PrevID != autoBase {
		t.Errorf("自动备份的上一环应仍是 %q（手动备份不入链），实际 %q", autoBase, inc.PrevID)
	}
}

// 链表不完整（包被手工删了）时必须重建基线：接在坏链上的增量，恢复时才发现找不回来。
func TestBrokenChainWritesNewBase(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL")

	baseName, _ := e.backup(t, autoBackupName, true)
	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	if _, m := e.backup(t, autoBackupName, true); m.Kind != "inc" {
		t.Fatalf("第二次应为增量，实际 %q", m.Kind)
	}

	// 模拟"用户把基线的包删了，只剩清单"
	if err := os.Remove(filepath.Join(e.bdir, baseName)); err != nil {
		t.Fatal(err)
	}
	_, m := e.backup(t, autoBackupName, true)
	if m.Kind != "base" {
		t.Errorf("链上丢了包之后必须重建基线，实际 kind=%q", m.Kind)
	}
}

// 不变式 3 的另一半：基线超过 7 天就重建（一天只备份一次时环数永远到不了 6）。
func TestStaleBaseWritesNewBase(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL")

	baseName, base := e.backup(t, autoBackupName, true)
	// 把基线清单的 created_at 改到 8 天前（包不动，只改时间）
	stale := base
	stale.CreatedAt = time.Now().Add(-maxChainAge - 24*time.Hour)
	raw, err := json.MarshalIndent(stale, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bdir, baseName+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	e.write(t, "world/level.dat", "LEVEL-v2-longer")
	_, m := e.backup(t, autoBackupName, true)
	if m.Kind != "base" {
		t.Errorf("基线超过 7 天必须重建基线，实际 kind=%q", m.Kind)
	}
}

// 备份**范围**变了（include_config 开关）不能接着旧链：上一环的清单里有配置项，
// 这一轮没扫，直接对比会把它们判成"已删除" —— 而墓碑在恢复时是真会执行的。
func TestIncludeConfigChangeWritesNewBase(t *testing.T) {
	e := newBackupTestEnv(t)
	e.write(t, "world/level.dat", "LEVEL")
	e.write(t, "server.properties", "motd=hi\n")

	if _, m := e.backup(t, autoBackupName, true); m.Kind != "base" {
		t.Fatalf("首次应为基线，实际 %q", m.Kind)
	}
	// 关掉"含配置"：范围变了
	_, m := e.backup(t, autoBackupName, false)
	if m.Kind != "base" {
		t.Errorf("include_config 变化时必须重建基线，实际 kind=%q", m.Kind)
	}
	if len(m.Deleted) != 0 {
		t.Errorf("范围变化的那一轮不该产生墓碑（否则恢复时会删掉配置），实际 %v", m.Deleted)
	}
}

// 往回走链必须有上限：清单是磁盘上的普通文件，prev_id 成环时不能挂死。
func TestChainStateStopsOnCycle(t *testing.T) {
	e := newBackupTestEnv(t)
	if err := os.MkdirAll(e.bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// a -> b -> a 的环，两份清单都存在（包也写出来，免得先因"包缺失"返回）
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(e.bdir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, prev string) {
		m := backupManifest{Schema: 1, Kind: "inc", PrevID: prev, CreatedAt: time.Now()}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.bdir, name+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "b")
	write("b", "a")

	m := readChainManifest(t, e.bdir, "a")
	if _, ok := e.srv.chainState(e.bdir, &m, "a"); ok {
		t.Error("成环的链不该被判定为可用（应返回 ok=false 让调用方重建基线）")
	}
}

// 同一秒内的两次备份不能重名退出（否则 os.Create 会截断上一份，链上那一环变成自己）。
func TestUniqueBackupFileNameAvoidsOverwrite(t *testing.T) {
	bdir := t.TempDir()
	ts := int64(1700000000)
	first, err := uniqueBackupFileName(bdir, ts, autoBackupName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, first), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := uniqueBackupFileName(bdir, ts, autoBackupName)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("同一秒的第二次备份拿到了同一个名字 %q（会覆盖上一份）", first)
	}
	// 名字仍要能被 parseBackupName 解析成"自动备份 + 时间戳"
	name, parsedTS := parseBackupName(second, time.Time{})
	if !isAutoBackupName(name) {
		t.Errorf("生成的名字 %q 解析出的名字 %q 未被识别为自动备份", second, name)
	}
	if parsedTS <= ts {
		t.Errorf("时间戳应往后挪（>%d），实际 %d", ts, parsedTS)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
