package httpapi

import "testing"

// 定时备份与手动备份必须传**同一个**备份根目录。
//
// 这条口径漏过一次：`scheduledBackup` 只传了 InstanceId/Name/IncludeConfig，
// 而 `BackupRoot` 只写在手动路径 `handleCreateBackup` 里。后果不是报错，是
// **自动备份悄悄写进实例目录所在的盘**（管理员配的冷存储只对手动备份生效）——
// 界面上一切正常：备份成功、列表里有、清单也对，只有去看落盘位置才发现。
//
// 2026-11-07 验收时就是这样发现的：单测全绿、接口全通，真机一看
// 自动备份落在 `instances/<id>/backups/` 而不是 `/mnt/cold-backup/<id>/`。
func TestAutoBackupRequestKeepsNodeBackupRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	if _, err := srv.db.Exec(
		`INSERT INTO nodes (id, name, ip, ssh_user, ssh_auth, backup_root) VALUES (7, 'n1', '127.0.0.1', 'root', '', '/mnt/cold-backup')`); err != nil {
		t.Fatalf("插入节点失败: %v", err)
	}
	if _, err := srv.db.Exec(
		`INSERT INTO instances (instance_id, node_id, name) VALUES ('inst-a', 7, 'a')`); err != nil {
		t.Fatalf("插入实例失败: %v", err)
	}

	req := srv.autoBackupRequest("inst-a", true)
	if req.InstanceId != "inst-a" {
		t.Errorf("InstanceId = %q", req.InstanceId)
	}
	// 名字是面板与节点之间的约定：保留策略靠它把手动备份排除在梯度淘汰之外，
	// 所以必须**显式**传 "auto"，不能指望 Daemon 的空名默认值。
	if req.Name != autoBackupName {
		t.Errorf("自动备份必须显式命名为 %q，实际 %q", autoBackupName, req.Name)
	}
	if !req.IncludeConfig {
		t.Error("include_config 应原样透传（这里传的是 true）")
	}
	if want := "/mnt/cold-backup"; req.BackupRoot != want {
		t.Errorf("BackupRoot = %q，应为节点配置的 %q（否则自动备份会写满实例所在的盘）", req.BackupRoot, want)
	}
	// 与手动路径同一口径：两边都取 s.nodeBackupRoot
	if req.BackupRoot != srv.nodeBackupRoot("inst-a") {
		t.Error("定时备份与手动备份的备份根目录口径不一致")
	}

	// include_config=false 也要照传（自动备份的范围由策略决定）
	if req := srv.autoBackupRequest("inst-a", false); req.IncludeConfig {
		t.Error("include_config=false 没有透传")
	}
}

// 节点没配备份根目录时，定时备份也不能凭空造一个值 ——
// 空表示「用实例目录下的 backups/」，由 Daemon 决定（节点侧兜底，面板不猜）。
func TestAutoBackupRequestWithoutNodeBackupRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	if _, err := srv.db.Exec(`INSERT INTO nodes (id, name, ip, ssh_user, ssh_auth) VALUES (8, 'n2', '127.0.0.1', 'root', '')`); err != nil {
		t.Fatalf("插入节点失败: %v", err)
	}
	if _, err := srv.db.Exec(
		`INSERT INTO instances (instance_id, node_id, name) VALUES ('inst-b', 8, 'b')`); err != nil {
		t.Fatalf("插入实例失败: %v", err)
	}
	if root := srv.autoBackupRequest("inst-b", true).BackupRoot; root != "" {
		t.Errorf("节点未配备份根目录时应留空（由 Daemon 回退到实例目录），实际 %q", root)
	}
}
