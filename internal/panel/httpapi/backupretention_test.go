package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/retention"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"

	"google.golang.org/grpc"
)

// fakeBackupDaemon 只实现备份相关的两个方法。
//
// 嵌入 nil 接口是刻意的：万一被测代码调了没实现的方法，会立刻 panic 而不是
// 静默走到一个"看起来正常"的空实现上 —— 测试里这种失败比漏测好得多。
type fakeBackupDaemon struct {
	pb.DaemonServiceClient
	backups []*pb.BackupInfo
	deleted []string
}

func (f *fakeBackupDaemon) ListBackups(context.Context, *pb.ListBackupsRequest, ...grpc.CallOption) (*pb.ListBackupsResponse, error) {
	return &pb.ListBackupsResponse{Success: true, Backups: f.backups}, nil
}

func (f *fakeBackupDaemon) DeleteBackup(_ context.Context, req *pb.DeleteBackupRequest, _ ...grpc.CallOption) (*pb.OperationResponse, error) {
	f.deleted = append(f.deleted, req.BackupId)
	return &pb.OperationResponse{Success: true, Message: "已删除"}, nil
}

// 手动备份（名字不是 auto）**不能**被自动备份的梯度规则淘汰。
//
// 这是本次修的 bug：面板「立即备份」的名称输入框写着"可选"，留空时 Daemon
// 会把它命名成 "auto"，于是这份**用户手动创建**的备份在保留策略眼里成了
// 自动备份，被梯度滚动淘汰掉 —— 用户看到的现象是"我的备份自己没了"。
//
// 这里把两种备份混在一起，断言"删掉的是哪些"而不是"删了几份"：
// 只看数量的话，删错了也能通过。
func TestPruneKeepsManualBackups(t *testing.T) {
	srv, _ := newTestServer(t)
	now := time.Now()

	mk := func(name string, ageHours int) *pb.BackupInfo {
		return &pb.BackupInfo{
			BackupId:  name + "-" + itoa(int64(ageHours)),
			Name:      name,
			CreatedAt: now.Add(-time.Duration(ageHours) * time.Hour).Unix(),
		}
	}

	f := &fakeBackupDaemon{backups: []*pb.BackupInfo{
		// 自动备份：最近 24h 有 6 份（策略只留 4），24~48h 有 1 份（留 2，所以留），
		// 60h 前 1 份（超最后一档 → 必删）
		mk("auto", 1), mk("auto", 2), mk("auto", 3), mk("auto", 4), mk("auto", 5), mk("auto", 6),
		mk("auto", 30),
		mk("auto", 60),
		// 手动备份：4 份，ManualKeep=3 → 只淘汰最旧的那一份
		mk("手动-20260918-100000", 1),
		mk("开荒前", 10),
		mk("手动-20260918-090000", 20),
		mk("正式服-第一周目", 40),
	}}

	policy := retention.Policy{
		ManualKeep: 3,
		Tiers:      []retention.Tier{{WithinHours: 24, Keep: 4}, {WithinHours: 48, Keep: 2}},
	}
	srv.pruneByPolicy(context.Background(), f, "inst-x", policy)

	deleted := map[string]bool{}
	for _, id := range f.deleted {
		deleted[id] = true
	}

	// 自动备份：24h 内 6 份只留最新的 4 份 → 5h/6h 那两份被删；60h 的那份必删
	mustDeleted := []string{"auto-5", "auto-6", "auto-60"}
	mustKeep := []string{
		"auto-1", "auto-2", "auto-3", "auto-4", // 24h 内保留的 4 份
		"auto-30", // 24~48h 档内只有 1 份，Keep=2 → 保留
		// 手动备份只淘汰最旧的一份（40h 那份），其余三份必须都在
		"开荒前-10", "手动-20260918-090000-20", "手动-20260918-100000-1",
	}
	// 上面 mustKeep 里最后一条的 id 由 mk 生成，核对命名口径
	for _, id := range mustDeleted {
		if !deleted[id] {
			t.Errorf("%s 应被淘汰（超出所属档位的保留份数），实际没删", id)
		}
	}
	for _, id := range mustKeep {
		if deleted[id] {
			t.Errorf("%s 不应被淘汰，实际被删了", id)
		}
	}
	if !deleted["正式服-第一周目-40"] {
		t.Error("最旧的手动备份应被 ManualKeep 淘汰，实际没删")
	}
	// 总数核对（防"多删了但恰好不在 mustKeep 里"）
	if len(f.deleted) != 4 {
		t.Errorf("应恰好淘汰 4 份（3 份自动 + 1 份手动），实际 %d 份: %v", len(f.deleted), f.deleted)
	}
}

// isAutoBackupName 必须**精确**匹配：用户给手动备份起名"自动化前测试"不该被当成自动备份。
func TestIsAutoBackupName(t *testing.T) {
	auto := []string{"auto", "AUTO", " auto ", "Auto"}
	notAuto := []string{"", "自动化前", "auto-backup", "autosave", "手动-auto", "备份-auto"}
	for _, s := range auto {
		if !isAutoBackupName(s) {
			t.Errorf("%q 应被识别为自动备份", s)
		}
	}
	for _, s := range notAuto {
		if isAutoBackupName(s) {
			t.Errorf("%q **不该**被识别为自动备份（前缀匹配会把用户的备份删掉）", s)
		}
	}
}

// 留空的手动备份名要自动生成，且**不能**是 auto。
//
// 这是修复的核心那一行：只要生成的名字不是 auto，这份备份就走手动通道，
// 不会被梯度规则滚动淘汰。
func TestManualBackupNameGenerated(t *testing.T) {
	now := time.Date(2026, 9, 18, 14, 30, 5, 0, time.UTC)
	name := manualBackupName(now)
	if name == "" {
		t.Fatal("手动备份名不能为空")
	}
	if isAutoBackupName(name) {
		t.Fatalf("生成的名字不能是 auto（否则会被当自动备份删掉），实际 %q", name)
	}
	if !strings.Contains(name, "20260918-143005") {
		t.Errorf("名字里应带创建时间（便于区分与排查），实际 %q", name)
	}
	// 两次生成（不同秒）必须不同名 —— 同名会覆盖前一份备份
	if name == manualBackupName(now.Add(time.Second)) {
		t.Error("不同时刻生成的名字应不同（同名会互相覆盖）")
	}
}
