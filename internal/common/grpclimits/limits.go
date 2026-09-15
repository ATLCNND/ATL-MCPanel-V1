// Package grpclimits 统一 gRPC 消息大小上限与上传体积上限。
//
// 为什么要单独一个包：这些数字必须**两端一致**，而它们原本散落各处 ——
// Daemon 的 server 没设上限（用 gRPC 默认的 4 MB）、应用层却写着 256 MB / 512 MB，
// 于是传一个 41 MB 的 jar 会收到一句
//
//	rpc error: code = ResourceExhausted desc = grpc: received message larger than max
//	(43616069 vs. 4194304)
//
// 这种报错对用户毫无意义：他看到的只是"上传失败"，而原因写在两个不同文件里。
// 放在同一个常量里，改一处就不会再对不上。
package grpclimits

const (
	// MaxUploadBytes 单个上传文件的应用层上限（jar / 共享资源）。
	//
	// 取 256 MB：Paper 这类核心约 50 MB，大型整合包服务端也很少超过 200 MB。
	// 再往上放的意义不大，而代价不小 —— 见下面的说明。
	MaxUploadBytes = 256 << 20

	// MaxMessageBytes 单条 gRPC 消息的上限，比上传上限留出余量
	//（protobuf 的字段头、分片开销等）。
	//
	// ⚠️ 当前的上传是**一次性的 unary 调用**，意味着整个文件会被完整地
	// 放进内存里两次：面板侧 ReadAll 一次 + 序列化一次，Daemon 侧反序列化一次。
	// 256 MB 的上限在这个前提下意味着单次上传可能占用 500 MB 以上的瞬时内存。
	//
	// 如果将来需要支持更大的文件（或并发上传），应当改成**分块流式上传**
	//（client-streaming RPC），而不是继续调大这两个数字。
	MaxMessageBytes = 320 << 20
)
