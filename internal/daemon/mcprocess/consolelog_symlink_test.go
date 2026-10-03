package mcprocess

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// secureTestDir 返回一个**解析过软链接**的临时目录。
//
// t.TempDir 在部分平台会经过软链接（如 macOS 的 /var -> /private/var），
// 而下面这些用例断言的正是"路径有没有被解析到别处"，先把基准解析成真实路径，
// 免得测出来的是平台差异而不是被测逻辑。
func secureTestDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// 控制台日志的软链接逃逸必须被拒绝。
//
// 2026-10-01 安全审查的 CRITICAL 发现：实例目录的内容完全由租户控制，
// 而 logs/console.log 是 **Daemon（root）自己打开**来当服务端 stdout 的文件。
// 把 `logs/console.log` 做成悬空软链接指向 /etc/cron.d/atl-x 之后，实例只要往
// 标准输出打印一行 crontab，root 就会替它建出那个文件（os.OpenFile 跟随软链接、
// O_CREATE 还会新建）—— 租户直接拿到节点 root。logs 目录本身是软链接时同理。
//
// 这条用例同时守住"没有把正常路径一起挡掉"：实例**内部**互指的软链接、
// 以及 logs 目录还不存在（全新实例，EvalSymlinks 会报 ENOENT）都必须放行，
// 否则加固的副作用是"所有新实例都起不来"。
func TestConsoleLogPathRejectsSymlinkedLogsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	root := secureTestDir(t)
	instDir := filepath.Join(root, "inst")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{instDir, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// ① logs 是指向实例外目录的软链接：`logs/console.log` 的词法路径看着
	//    还在实例目录里，实际已经落到别处
	if err := os.Symlink(outside, filepath.Join(instDir, "logs")); err != nil {
		t.Fatal(err)
	}
	if got, err := consoleLogPath(instDir, "t1"); err == nil {
		t.Errorf("logs 是软链接时应拒绝，却返回 %q", got)
	}

	// ② 实例内互指的软链接不能误伤（logs -> real-logs 是正常用法）
	if err := os.Remove(filepath.Join(instDir, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(instDir, "real-logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-logs", filepath.Join(instDir, "logs")); err != nil {
		t.Fatal(err)
	}
	got, err := consoleLogPath(instDir, "t1")
	if err != nil {
		t.Fatalf("实例内互指的软链接被误伤：%v", err)
	}
	if want := filepath.Join(instDir, "logs", "console.log"); got != want {
		t.Errorf("consoleLogPath = %q，期望 %q", got, want)
	}

	// ③ logs 目录还不存在时也要放行（全新实例的第一次启动）
	fresh := filepath.Join(root, "fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := consoleLogPath(fresh, "t2"); err != nil {
		t.Errorf("logs 目录尚不存在时被误伤：%v", err)
	}
}

// 以 root 打开控制台日志（追加）时必须拒绝软链接目标 —— 这是那条提权路径的
// 最后一步：只要 open 跟随了软链接，实例打印的任意内容就会落进租户指定的文件。
func TestOpenConsoleLogAppendRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Daemon 只发布 Linux 版本（O_NOFOLLOW）")
	}
	root := secureTestDir(t)
	instDir := filepath.Join(root, "inst")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(instDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(instDir, "logs", "console.log")

	// ① 悬空软链接：拒绝打开，且**不能**在实例外建出文件
	dangling := filepath.Join(outside, "atl-x")
	if err := os.Symlink(dangling, logPath); err != nil {
		t.Fatal(err)
	}
	f, err := openConsoleLogAppend(logPath, "t1")
	if err == nil {
		f.Close()
		t.Fatal("目标是悬空软链接时本应拒绝打开（否则 root 会替租户建出那个文件）")
	}
	if _, serr := os.Stat(dangling); !os.IsNotExist(serr) {
		t.Error("实例外的文件被以 root 建出来了")
	}

	// ② 指向已存在文件的软链接：拒绝打开，且目标内容不能被追加
	victim := filepath.Join(outside, "victim.conf")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, logPath); err != nil {
		t.Fatal(err)
	}
	f, err = openConsoleLogAppend(logPath, "t1")
	if err == nil {
		f.Close()
		t.Fatal("指向已存在文件的软链接同样必须拒绝")
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Errorf("实例外的文件被写入了：%q", string(b))
	}

	// ③ 普通文件要照旧能打开并追加（安全修复不能把功能一起挡掉）
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	f, err = openConsoleLogAppend(logPath, "t1")
	if err != nil {
		t.Fatalf("普通日志文件被误伤：%v", err)
	}
	if _, werr := f.WriteString("hello\n"); werr != nil {
		t.Errorf("追加写入失败：%v", werr)
	}
	f.Close()
	if b, err := os.ReadFile(logPath); err != nil || string(b) != "hello\n" {
		t.Errorf("写入结果不对：%q err=%v", string(b), err)
	}
}

// 控制台历史回放（RecentOutput）同样是以 root 读，所以也不能跟着软链接读出去：
// `logs/console.log -> /etc/shadow` 会把节点机密送回该实例的控制台。
func TestRecentOutputRefusesSymlinkedConsoleLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Daemon 只发布 Linux 版本（O_NOFOLLOW）")
	}
	root := secureTestDir(t)
	instDir := filepath.Join(root, "inst")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(instDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(instDir, "logs", "console.log")
	if err := os.Symlink(secret, logPath); err != nil {
		t.Fatal(err)
	}

	inst := testInstance("t1", instDir, "", "1G", "1G")
	if got := inst.RecentOutput(10); got != nil {
		t.Errorf("控制台回放不能通过软链接读到实例外的文件，实际读到 %v", got)
	}

	// 正常日志仍要能回放
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := inst.RecentOutput(10)
	if len(got) != 2 || got[1] != "line2\n" {
		t.Errorf("正常回放被误伤：%v", got)
	}
}

// 轮转不能借软链接动实例外的文件（os.Stat / os.Rename / os.Remove 都跟随软链接），
// 检测到软链接就整体放弃轮转 —— 代价只是"日志不滚动"。
func TestRotateBySizeRefusesSymlinkedHistory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Daemon 只发布 Linux 版本")
	}
	root := secureTestDir(t)
	instDir := filepath.Join(root, "inst")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{instDir, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(instDir, "console.log")
	if err := os.WriteFile(logPath, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "keep.conf")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 历史文件是软链接：轮转要按编号推移并删除最旧的那个
	if err := os.Symlink(victim, logPath+".3"); err != nil {
		t.Fatal(err)
	}

	rotateBySize(logPath, 1024, 3)
	if _, err := os.Stat(logPath + ".1"); err == nil {
		t.Error("检测到软链接时应整体放弃轮转，实际滚动出了 console.log.1")
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep" {
		t.Errorf("实例外的文件被轮转影响：%q err=%v", string(b), err)
	}

	// 清掉软链接后轮转要照旧工作（加固不能让它永久停摆）
	if err := os.Remove(logPath + ".3"); err != nil {
		t.Fatal(err)
	}
	rotateBySize(logPath, 1024, 3)
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Errorf("正常轮转被误伤：%v", err)
	}
}
