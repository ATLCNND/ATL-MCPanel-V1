package mcprocess

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// server.properties 的软链接写入必须被拒绝（2026-10-01 安全审查发现）。
//
// 这个文件在实例目录里、租户随手可改，而 syncServerPort 是以 **root** 的身份
// 去读它、改写它。租户放一个 `server.properties -> /etc/ld.so.preload` 的软链接，
// 下次启动实例时 Daemon 就会以 root 建出/改写那个文件（写进 ld.so.preload 会让
// 所有动态链接的程序都起不来，等于把整台节点打停）。
//
// 期望的行为是"跳过校准 + 记警告"，不是报错：端口校准只是修正一致性的动作，
// 失败不该拦住实例启动；但**绝不能**写穿软链接。
func TestSyncServerPortRefusesSymlinkedProps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	root := secureTestDir(t)
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	// ① 悬空软链接：不能以 root 在实例外建出文件
	dir := filepath.Join(root, "inst-dangling")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(outside, "ld.so.preload")
	if err := os.Symlink(dangling, filepath.Join(dir, serverPropsFile)); err != nil {
		t.Fatal(err)
	}
	changed, _, err := syncServerPort(dir, 25567, false)
	if err != nil {
		t.Fatalf("目标是软链接时应安静跳过（端口校准不是启动前置条件），实际报错：%v", err)
	}
	if changed {
		t.Error("目标是软链接时不应报告已改写文件")
	}
	if _, serr := os.Stat(dangling); !os.IsNotExist(serr) {
		t.Error("实例外的文件被以 root 建出来了")
	}

	// ② 指向已存在文件的软链接：目标内容必须原样不动（否则等于以 root
	//    改写节点上的任意文件）
	dir2 := filepath.Join(root, "inst-existing")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "real.conf")
	if err := os.WriteFile(victim, []byte("other-config=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir2, serverPropsFile)); err != nil {
		t.Fatal(err)
	}
	changed, _, err = syncServerPort(dir2, 25567, false)
	if err != nil {
		t.Fatalf("目标是软链接时应安静跳过，实际报错：%v", err)
	}
	if changed {
		t.Error("目标是软链接时不应报告已改写文件")
	}
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "other-config=1\n" {
		t.Errorf("实例外的文件被改写了：%q err=%v", string(b), rerr)
	}

	// ③ 普通文件仍要照旧校准（加固不能把功能一起挡掉）
	dir3 := filepath.Join(root, "inst-normal")
	if err := os.MkdirAll(dir3, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProps(t, dir3, "server-port=25565\n")
	changed, old, err := syncServerPort(dir3, 25567, false)
	if err != nil || !changed || old != 25565 {
		t.Fatalf("普通文件应照旧校准，changed=%v old=%d err=%v", changed, old, err)
	}
	if got := readProps(t, dir3); !strings.Contains(got, "server-port=25567") {
		t.Errorf("端口未被校准，实际内容：%q", got)
	}
}
