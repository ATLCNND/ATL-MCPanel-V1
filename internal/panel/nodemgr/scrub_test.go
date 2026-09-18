package nodemgr

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 节点返回的错误里带绝对路径时，对外必须被换掉。
//
// 内测报告里的实例：
//
//	GET /api/instances/beta03/download?path=/etc/passwd
//	→ 500 {"error":"...stat /opt/atl-node/instances/beta03/etc/passwd: no such file..."}
//
// 有了这条闸，攻击者拿不到部署布局；细节仍然在面板日志里。
func TestScrubInterceptorHidesNodePaths(t *testing.T) {
	leaky := status.Error(codes.Unknown,
		"读取文件失败：stat /opt/atl-node/instances/beta03/etc/passwd: no such file or directory")

	invoker := func(context.Context, string, interface{}, interface{}, *grpc.ClientConn, ...grpc.CallOption) error {
		return leaky
	}
	err := scrubInterceptor(context.Background(), "/mcpanel.DaemonService/ReadFile",
		nil, nil, nil, invoker)
	if err == nil {
		t.Fatal("错误应被透出（只是内容被换掉）")
	}
	msg := err.Error()
	for _, leak := range []string{"/opt/atl-node", "instances", "beta03", "passwd", "stat "} {
		if strings.Contains(msg, leak) {
			t.Errorf("对外错误里不该出现 %q，实际：%s", leak, msg)
		}
	}
	// 仍然要能看出"是哪一类失败"，否则用户只能干瞪眼
	if !strings.Contains(msg, "节点") {
		t.Errorf("应保留「节点侧失败」这个可判断信息，实际：%s", msg)
	}

	// 成功路径不该被改动
	okInvoker := func(context.Context, string, interface{}, interface{}, *grpc.ClientConn, ...grpc.CallOption) error {
		return nil
	}
	if err := scrubInterceptor(context.Background(), "m", nil, nil, nil, okInvoker); err != nil {
		t.Errorf("成功调用不该被拦截器改动，实际 %v", err)
	}
}

// 语义明确、又不含内部细节的错误码原样透出 —— 否则用户拿到的提示会变得没用。
//
// 例如"实例未运行"这种 FailedPrecondition，前端要按它给出可操作的建议；
// 换成"节点执行失败：请稍后重试"反而是退步。
func TestScrubInterceptorKeepsActionableCodes(t *testing.T) {
	for _, c := range []codes.Code{
		codes.NotFound, codes.PermissionDenied, codes.Unauthenticated,
		codes.ResourceExhausted, codes.AlreadyExists, codes.FailedPrecondition,
		codes.Unimplemented, codes.DeadlineExceeded,
	} {
		orig := status.Error(c, "实例未运行")
		invoker := func(context.Context, string, interface{}, interface{}, *grpc.ClientConn, ...grpc.CallOption) error {
			return orig
		}
		got := scrubInterceptor(context.Background(), "m", nil, nil, nil, invoker)
		if got == nil || got.Error() != orig.Error() {
			t.Errorf("错误码 %v 应原样透出，实际 %v", c, got)
		}
	}
}
