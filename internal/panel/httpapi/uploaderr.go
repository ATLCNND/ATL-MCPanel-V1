package httpapi

import (
	"fmt"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
)

// friendlyUploadErr 把上传时的 gRPC 错误翻译成人能看懂的话。
//
// 为什么需要它：gRPC 的传输层与应用的体积上限原本各写各的，一旦撞上传输层
// 限制，用户看到的是一句
//
//	rpc error: code = ResourceExhausted desc = grpc: received message larger than max
//	(43616069 vs. 4194304)
//
// 这句话里没有任何"你该怎么做"的信息 —— 既没说是哪个限制，
// 也没说上传上限到底是多少。现在两端上限已经统一（grpclimits），
// 但万一将来又不一致、或者文件确实超限，这里至少能给出明确的原因与数字。
func friendlyUploadErr(err error, size int64) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "ResourceExhausted") || strings.Contains(msg, "larger than max") {
		return fmt.Sprintf(
			"文件过大（%.1f MB），超过了节点允许的单次上传上限（%.0f MB）。"+
				"请换用更小的文件，或先在节点上手动放置该文件再引用它。原始错误：%s",
			float64(size)/1024/1024,
			float64(grpclimits.MaxUploadBytes)/1024/1024,
			msg,
		)
	}
	return msg
}
