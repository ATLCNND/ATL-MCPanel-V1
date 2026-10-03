package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// noteConsoleInterrupted 是另一条"以 root 往租户可控路径追加写"的路径。
//
// 它由 registerContainerInstance 在"控制台输出接不上"时调用，往
// <实例目录>/logs/console.log 追加一句平台说明。实例目录里的软链接由租户随意
// 创建，所以这里必须与 mcprocess 里的控制台日志出入口保持同一套把关：
// `logs -> /etc` 或 `logs/console.log -> /etc/cron.d/atl-x` 都会让这句说明
// 落进节点上的任意文件（os.OpenFile 跟随软链接，O_CREATE 还会新建）。
//
// 失败**只记日志**（写不进去不该让实例注册不上），所以这里断言的是"没有写出去"，
// 而不是"返回了错误"。
func TestNoteConsoleInterruptedRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "inst")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(dir, "logs"), outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// ① 悬空软链接：说明不能被以 root 写进实例外的文件
	dangling := filepath.Join(outside, "atl-x")
	if err := os.Symlink(dangling, filepath.Join(dir, "logs", "console.log")); err != nil {
		t.Fatal(err)
	}
	noteConsoleInterrupted(dir, "t1")
	if _, serr := os.Stat(dangling); !os.IsNotExist(serr) {
		t.Error("实例外的文件被以 root 建出来了")
	}

	// ② 指向已存在文件的软链接：目标内容必须原样不动
	victim := filepath.Join(outside, "victim.conf")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "logs", "console.log")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, logPath); err != nil {
		t.Fatal(err)
	}
	noteConsoleInterrupted(dir, "t1")
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "original" {
		t.Errorf("实例外的文件被写入了：%q err=%v", string(b), rerr)
	}

	// ③ 正常情况仍要写进控制台（这条兜底文案是给人看的，不能因加固而消失）
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	noteConsoleInterrupted(dir, "t1")
	b, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("普通日志文件被误伤：%v", rerr)
	}
	if !strings.Contains(string(b), "平台") {
		t.Errorf("控制台说明未写入，实际内容：%q", string(b))
	}
}
