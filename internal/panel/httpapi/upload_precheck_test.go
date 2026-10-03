package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
)

// 上传预检必须拒绝**长度未知**的请求。
//
// 背景（这轮发现的缺口）：面板把 r.ContentLength 直接当体积传进来，而
// `Transfer-Encoding: chunked` 的请求里它恒为 -1 —— 老实现是
// `if size <= 0 { return true, "" }`，等于把"大小未知"当成"肯定没问题"：
// 实例磁盘配额与节点剩余空间**两道闸门一起被跳过**，只剩 Daemon 的 256MB
// 单文件上限，而单文件上限拦不住"反复传" —— 租户能在调度器下一轮检查前把
// 节点磁盘写满，受害的是同节点的其他租户，还有面板自己的 SQLite WAL 与 Daemon 日志。
//
// 这里刻意传 nil 客户端：长度未知的分支必须**在任何查库 / 连 Daemon 之前**就拒绝，
// 否则一个注定被拒的分块请求还要先连一次节点才知道行不行。
func TestUploadPrecheckRejectsUnknownLength(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()

	// -1：chunked 请求的长度（长度未知）—— 必须拒绝
	ok, reason := srv.uploadPrecheck(ctx, nil, "ghost-instance", -1)
	if ok {
		t.Fatal("长度未知的请求不该通过预检：这正是分块上传绕过两道磁盘闸门的地方")
	}
	if !strings.Contains(reason, "Content-Length") {
		t.Errorf("拒绝理由要说清是缺 Content-Length，而不是笼统的失败提示：%s", reason)
	}

	// 0：Content-Length: 0，是"确实为空"而不是"长度未知"（Go 服务端语义），
	// 不该在这一步被当成未知拦下。库里的实例不存在，后面的配额/余量检查查不到
	// 实例记录就会放行 —— 这里只验证它不再走"未知长度"那条分支。
	if ok, reason := srv.uploadPrecheck(ctx, nil, "ghost-instance", 0); !ok {
		t.Errorf("空文件不该因为体积未知被拒（0 与 -1 的语义不同）：%s", reason)
	}

	// 单文件上限的边界保持原样
	if ok, reason := srv.uploadPrecheck(ctx, nil, "ghost-instance", grpclimits.MaxUploadBytes); !ok {
		t.Errorf("正好等于单文件上限应放行：%s", reason)
	}
	if ok, reason := srv.uploadPrecheck(ctx, nil, "ghost-instance", grpclimits.MaxUploadBytes+1); ok {
		t.Error("超过单文件上限必须拒绝")
	} else if !strings.Contains(reason, "文件过大") {
		t.Errorf("超限的拒绝理由应说明是文件过大：%s", reason)
	}
}
