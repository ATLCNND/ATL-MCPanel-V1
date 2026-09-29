package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// uploadChunkSize 上传分片大小，与 Daemon 的 downloadChunkSize 对齐（1MB）。
//
// 1MB 是吞吐与内存的折中：太小会让 gRPC 调用次数暴涨；太大则每个并发上传
// 都要占住一大块缓冲 —— 而"拖动上传多个文件"天然会并发。
const uploadChunkSize = 1 << 20

// handleUploadFile POST /api/instances/{id}/upload?path=<实例内相对路径>[&overwrite=1]
//
// 请求体就是**文件的原始字节**（不是 multipart）：
//   - 面板用 URL 参数传路径，body 直通 Daemon，中间不做任何解析/缓冲 ——
//     一个几百 MB 的模组包不该在面板内存里过一遍；
//   - 前端用 XHR 发这个请求，就能拿到 upload.onprogress（fetch 目前没有上传进度回调）。
//
// 权限与其它文件写入一致（LevelOwner）：上传等于往实例目录里写东西，
// 能写就等于能放插件 jar（也就是以实例身份执行代码），不该比编辑文本文件更宽松。
//
// 路径安全**不在这一层做**：真正的边界在 Daemon 的 resolvePath（防 ../ 与软链接穿越）
// 与 isProtectedPath（挡住 instance.json 等平台文件）。面板这层只做体积预检。
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if relPath == "" {
		writeErr(w, http.StatusBadRequest, "缺少 path 参数（目标文件在实例内的相对路径）")
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "1"

	// 体积预检：Content-Length 可信时先挡一次，省得白传几百 MB 再被 Daemon 拒绝。
	// 拿不到（分块传输）就交给 Daemon 那边按实际上限流式拒绝。
	if r.ContentLength > grpclimits.MaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"文件过大：单文件上限 "+humanBytes(grpclimits.MaxUploadBytes))
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := cli.UploadFile(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法建立上传通道："+err.Error())
		return
	}

	// 第一片必须带上实例与目标路径（Daemon 用它建临时文件、校验路径）
	if err := stream.Send(&pb.UploadChunk{
		InstanceId: instanceID,
		Path:       relPath,
		Total:      r.ContentLength,
		Overwrite:  overwrite,
	}); err != nil {
		writeErr(w, http.StatusBadGateway, "上传初始化失败："+err.Error())
		return
	}

	buf := make([]byte, uploadChunkSize)
	var sent int64
	for {
		n, readErr := r.Body.Read(buf)
		if n > 0 {
			// 复制一份再发：buf 会被下一轮 Read 覆盖，而 gRPC 发送是异步的
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sent += int64(n)
			if sent > grpclimits.MaxUploadBytes {
				_, _ = stream.CloseAndRecv()
				writeErr(w, http.StatusRequestEntityTooLarge,
					"文件过大：单文件上限 "+humanBytes(grpclimits.MaxUploadBytes))
				return
			}
			if err := stream.Send(&pb.UploadChunk{Data: chunk}); err != nil {
				// Daemon 提前拒了（路径非法、已存在且不允许覆盖…）：把那句话原样带回去
				writeErr(w, http.StatusBadRequest, uploadErrText(err))
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// 客户端中断（用户取消/断网）：关掉流让 Daemon 清掉临时文件
			_, _ = stream.CloseAndRecv()
			writeErr(w, http.StatusBadRequest, "读取上传内容失败："+readErr.Error())
			return
		}
	}

	res, err := stream.CloseAndRecv()
	if err != nil {
		// 这里是**客户端原因**占多数：路径受保护（instance.json）、目标已存在且未允许覆盖、
		// 超出上限……Daemon 会带一句明确的中文原因回来。
		// 用 400 而不是 502：502 会让前端与运维都以为是"服务端故障"，
		// 而这类拒绝对用户来说就是"这个请求本身不行"，得能看懂、能改。
		writeErr(w, http.StatusBadRequest, uploadErrText(err))
		return
	}
	if !res.Success {
		writeErr(w, http.StatusBadRequest, res.Error)
		return
	}

	s.audit(r, "upload_file", instanceID, relPath+"（"+humanBytes(res.Size)+"）")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "已上传 " + relPath,
		"path":    res.Path,
		"size":    res.Size,
	})
}

// uploadErrText 把 gRPC 错误压成一句能给用户看的话。
//
// Daemon 那边拒绝的原因（路径非法、文件已存在、超限）都是**用户需要知道的**，
// 而 gRPC 会把它们包成 `rpc error: code = Unknown desc = <原话>`；
// 直接把整串抛给界面既啰嗦又容易被误认为系统故障。
func uploadErrText(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "desc = "); i >= 0 {
		msg = msg[i+len("desc = "):]
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "上传失败"
	}
	return "上传失败：" + msg
}
