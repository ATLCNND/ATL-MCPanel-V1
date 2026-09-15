package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteUserRemovesAvatarFile 删账号时必须把**头像文件**也删掉。
//
// 背景（真实发现）：头像只存在磁盘上（data/avatars/<uid><ext>），
// 删 users 那一行不会连带删文件。原来的 handleDeleteUser 漏了这一步，
// 于是在生产库里积了 4 个孤儿文件（每删一个有头像的账号就多一个）。
// 对比：handleDeleteMyAvatar（自己删头像）是删文件的，只有管理员删账号这条路径漏了。
func TestDeleteUserRemovesAvatarFile(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "av1", "password": "av123456", "role": "user"})

	uid := uidOf(t, srv, "av1")

	// 直接造成"已上传且审核通过"的状态：写 DB 字段 + 落一个真实文件
	file := fmt.Sprintf("%d.png", uid)
	if err := os.MkdirAll(srv.avatarDir, 0o755); err != nil {
		t.Fatalf("建头像目录失败: %v", err)
	}
	path := filepath.Join(srv.avatarDir, file)
	if err := os.WriteFile(path, []byte("fake-png"), 0o644); err != nil {
		t.Fatalf("写头像文件失败: %v", err)
	}
	if _, err := srv.db.Exec(
		`UPDATE users SET avatar_file = ?, avatar_status = 'approved' WHERE id = ?`, file, uid); err != nil {
		t.Fatalf("更新头像字段失败: %v", err)
	}

	code, body := doJSON(t, ts, "DELETE", "/api/users/"+itoa(uid), adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("删账号应 200，实际 %d body=%v", code, body)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("删账号后头像文件应被删除，实际仍存在（stat err=%v）", err)
	}
	// 账号本身当然也要没了
	var n int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, uid).Scan(&n)
	if n != 0 {
		t.Errorf("账号应已删除，实际还剩 %d 行", n)
	}
}

// TestDeleteUserKeepsAvatarOfOthers 别删错人：只删目标账号的文件。
func TestDeleteUserKeepsAvatarOfOthers(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	for _, u := range []string{"av2", "av3"} {
		doJSON(t, ts, "POST", "/api/users", adminTok,
			map[string]string{"username": u, "password": "av123456", "role": "user"})
	}
	if err := os.MkdirAll(srv.avatarDir, 0o755); err != nil {
		t.Fatalf("建头像目录失败: %v", err)
	}
	paths := map[string]string{}
	for _, u := range []string{"av2", "av3"} {
		uid := uidOf(t, srv, u)
		file := fmt.Sprintf("%d.png", uid)
		p := filepath.Join(srv.avatarDir, file)
		if err := os.WriteFile(p, []byte("fake-png"), 0o644); err != nil {
			t.Fatalf("写头像失败: %v", err)
		}
		if _, err := srv.db.Exec(
			`UPDATE users SET avatar_file = ?, avatar_status = 'approved' WHERE id = ?`, file, uid); err != nil {
			t.Fatalf("更新头像字段失败: %v", err)
		}
		paths[u] = p
	}

	doJSON(t, ts, "DELETE", "/api/users/"+itoa(uidOf(t, srv, "av2")), adminTok, nil)

	if _, err := os.Stat(paths["av2"]); !os.IsNotExist(err) {
		t.Errorf("被删账号的头像应消失")
	}
	if _, err := os.Stat(paths["av3"]); err != nil {
		t.Errorf("未被删账号的头像不该被动到: %v", err)
	}
}
