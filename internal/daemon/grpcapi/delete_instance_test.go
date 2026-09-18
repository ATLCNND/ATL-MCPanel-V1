package grpcapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// TestDeleteInstanceIdempotentWhenNotRegistered 回归测试：
// 节点上**没有**这个实例时（实例目录已丢失、节点重装过、或只是库里的孤儿记录），
// "删除"必须返回**成功** —— 因为目标状态已经达成。
//
// 背景（真实踩到的 bug）：原实现直接调 registry.Delete，
// 而它在实例未注册时返回错误「实例不存在」；面板又只在 resp.Success 时才
// 清理数据库记录 —— 于是"节点上已无此实例"的孤儿记录**永远删不掉**，
// 白占实例 ID 与游戏端口。生产库里实测积了 3 条（实例名为「验收…」，
// 节点目录早已不在）。
//
// 这个测试同时守住"没有放宽过头"：见下面第二个用例。
func TestDeleteInstanceIdempotentWhenNotRegistered(t *testing.T) {
	base := t.TempDir()
	stateDir := t.TempDir()
	srv := NewServer(&config.DaemonConfig{
		InstanceDir: base,
		StateDir:    stateDir,
		FrpStateDir: t.TempDir(),
	}, registry.New(base, stateDir), logger.New("error"), nil, nil)
	ctx := context.Background()

	// ① 未注册、不删文件 → 成功
	resp, err := srv.DeleteInstance(ctx, &pb.DeleteInstanceRequest{InstanceId: "ghost"})
	if err != nil {
		t.Fatalf("DeleteInstance 返回错误: %v", err)
	}
	if !resp.Success {
		t.Errorf("未注册的实例应视为已删除（幂等），实际返回失败: %s", resp.Error)
	}

	// ② 未注册 + RemoveFiles → 仍要清掉磁盘上的残留目录
	ghostDir := filepath.Join(base, "ghost2")
	if err := os.MkdirAll(ghostDir, 0o755); err != nil {
		t.Fatalf("建残留目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ghostDir, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写残留文件失败: %v", err)
	}
	resp, err = srv.DeleteInstance(ctx, &pb.DeleteInstanceRequest{InstanceId: "ghost2", RemoveFiles: true})
	if err != nil {
		t.Fatalf("DeleteInstance 返回错误: %v", err)
	}
	if !resp.Success {
		t.Errorf("带 RemoveFiles 的孤儿删除也应成功，实际: %s", resp.Error)
	}
	if _, err := os.Stat(ghostDir); !os.IsNotExist(err) {
		t.Errorf("残留目录应被一并清理，实际仍存在（stat err=%v）", err)
	}

	// ③ 防呆：陈旧/可疑目录名不得被 RemoveAll 波及
	//    （base 目录本身必须还在，绝不能被当成实例目录删掉）
	if _, err := os.Stat(base); err != nil {
		t.Errorf("实例根目录不应被删除: %v", err)
	}
}
