package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// ---- 写入前预检：单文件上限 + 实例磁盘软配额 + 节点剩余空间 ----
	//
	// 为什么必须在**写之前**算这笔账：磁盘限额目前只由调度器事后强制，而它的动作是
	// "超限就把实例停机"（scheduler.checkDiskLimit）—— 对上传来说这个兜底太晚：
	// 单文件 256MB、请求不限次数，租户能在调度器下一轮检查前把节点磁盘写满，
	// **受害的是同节点的其他租户**；配额为 0（默认）时更是完全没有限制。
	//
	// 口径与「上传前预检」接口共用同一个 uploadPrecheck：两边判断不一致
	//（预检说可以、真传却被拒）比不预检更让人困惑。
	//
	// 注意：客户端在发送大文件的过程中被拒会看到 connection reset 而不是这句提示，
	// 所以界面必须先调 /upload-check 再发 —— 这里的检查是防竞态的第二道。
	if ok, reason := s.uploadPrecheck(ctx, cli, instanceID, r.ContentLength); !ok {
		drainBody(r, drainBeforeReject)
		// 长度未知（ContentLength 为负）要回 400 而不是 413：413 会把人引向
		// "文件太大"，而真正的原因是这次请求没声明 Content-Length，
		// 用户再怎么改文件大小也没用。
		if r.ContentLength < 0 {
			writeErr(w, http.StatusBadRequest, reason)
			return
		}
		writeErr(w, http.StatusRequestEntityTooLarge, reason)
		return
	}

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
				// Daemon 提前拒了（路径非法、已存在且不允许覆盖…）：把那句话原样带回去。
				//
				// 注意不能直接用 err 的文本：流式 RPC 里服务端带错误状态提前返回时，
				// 客户端下一次 Send 拿到的是 **io.EOF**，真正的原因要用 CloseAndRecv
				// 把状态取回来 —— 否则用户看到的是毫无信息量的"上传失败：EOF"，
				// 还容易被当成网络故障去排查（实测就是这样）。
				writeErr(w, http.StatusBadRequest, uploadStreamErr(stream, err))
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			// 客户端中断（用户取消/断网）：**取消流**让 Daemon 看到 context canceled，
			// 从而清掉临时文件、不碰目标文件。
			//
			// 这里刻意**不调 CloseAndRecv**：那只是"我发完了"的半关闭，
			// Daemon 收到的是 io.EOF —— 与正常发完无法区分，会把收到的半截内容
			// 改名到目标路径（插件 jar 变成半截，比报错严重得多）。
			// Daemon 侧另有长度自检兜底，两层都要有。
			cancel()
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

// instanceDiskLimitMB 读实例的磁盘软配额（MB）。ok=false 表示查不到该实例。
//
// 0 表示"不限制"（历史默认值），此时不做配额预检 —— 但 Daemon 侧的
// "节点剩余空间"检查仍然生效，那是保护整台机器的最后一道。
func (s *Server) instanceDiskLimitMB(instanceID string) (int64, bool) {
	var limit int64
	if err := s.db.QueryRow(
		`SELECT COALESCE(disk_limit_mb, 0) FROM instances WHERE instance_id = ?`,
		instanceID).Scan(&limit); err != nil {
		return 0, false
	}
	return limit, true
}

// quotaExceeded 纯函数形式的配额判断（便于单测与复用）。
//
// 口径：**已用 + 本次 > 配额** 才算超。正好等于不拦（软配额允许用满）。
func quotaExceeded(limitMB, usedBytes, incomingBytes int64) (bool, string) {
	if limitMB <= 0 || incomingBytes <= 0 {
		return false, ""
	}
	limitBytes := limitMB << 20
	if usedBytes+incomingBytes <= limitBytes {
		return false, ""
	}
	return true, "磁盘配额不足：该实例配额 " + humanBytes(limitBytes) +
		"，已用 " + humanBytes(usedBytes) +
		"，本次上传 " + humanBytes(incomingBytes) +
		" 会超出。请先清理或联系管理员调整配额。"
}

// handleUploadCheck GET /api/instances/{id}/upload-check?size=<字节>
//
// **上传前的预检**。为什么需要单独一个接口（而不是只在上传时返回 413）：
// 服务端在收到大文件的过程中提前拒绝、直接关连接的话，客户端还在往外发数据，
// 结果是一个 **connection reset** —— 用户看到的是"网络被重置"，而不是
// "磁盘配额不足：配额 2 GB，已用 1.9 GB"。实测就是这样（20MB 的请求被拒时
// Python/wget 都拿到 RST 而非响应体）。
//
// 所以：界面在**开始发送前**先问一次，拿不到许可就不发；
// 上传路径里的同名检查仍然保留（防并发与竞态），但那已经是极少数情况。
func (s *Server) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var size int64
	if v := r.URL.Query().Get("size"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &size); err != nil || size < 0 {
			writeErr(w, http.StatusBadRequest, "size 参数不合法")
			return
		}
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if ok, reason := s.uploadPrecheck(ctx, cli, instanceID, size); !ok {
		// 200 + ok:false 而不是 4xx：这不是"请求错了"，而是"答案是不行"，
		// 前端要把 reason 原样展示在对应文件那一行上。
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// uploadPrecheck 上传能否进行：单文件上限 + 实例磁盘配额 + 节点剩余空间。
//
// **一处实现、两处调用**（预检接口与上传本身），避免两边判断口径漂移 ——
// 那种"预检说可以、真传却被拒"的不一致比不预检更让人困惑。
//
// 长度未知（分块传输）也在这里拒绝，不再"未知就先放行"：见下面 size < 0 处的说明。
func (s *Server) uploadPrecheck(ctx context.Context, cli pb.DaemonServiceClient,
	instanceID string, size int64) (bool, string) {

	if size > grpclimits.MaxUploadBytes {
		return false, "文件过大：单文件上限 " + humanBytes(grpclimits.MaxUploadBytes)
	}
	// size < 0 = **长度未知**：Transfer-Encoding: chunked 时 r.ContentLength 恒为 -1。
	//
	// 这里以前是 `if size <= 0 { return true, "" }`（"大小未知就交给 Daemon 边传边判"），
	// 后果是下面两道闸门被**一起**跳过：磁盘配额要算"已用 + 本次"、节点剩余空间要算
	// "可用 - 本次"，两个都必须知道本次体积。于是分块上传只受 Daemon 的 256MB
	// 单文件上限约束 —— 而单文件上限拦不住"反复传"：租户能在调度器下一轮检查前
	// 把节点磁盘写满，**受害的是同节点的其他租户**，还有面板自己的 SQLite WAL
	// 与 Daemon 的日志。
	//
	// 所以长度未知一律拒绝，而不是"未知就先放行"。浏览器的上传路径必然带
	// Content-Length（0 表示请求体确实一个字节都没有），正常上传不受影响。
	if size < 0 {
		return false, "无法确定上传体积：请求未声明 Content-Length（分块传输）。" +
			"请使用界面上的文件上传，或改为携带 Content-Length 的请求。"
	}

	// 注意这里**不再**对 size == 0 提前放行：0 是"确实是空文件"，体积账算出来是
	// "不需要新增空间"，但节点余量已经低于预留值时同样不该再往里写。
	//
	// 实例的磁盘**软配额**：面板自己的库里有这个值，但要做减法得知道已用量，
	// 那只有 Daemon 知道（GetInstanceRuntime 的 disk_used/disk_free）。
	limitMB, ok := s.instanceDiskLimitMB(instanceID)
	if !ok {
		return true, "" // 查不到实例记录就不拦（正常流程不会走到）
	}
	rt, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID})
	if err != nil || rt == nil || !rt.Success {
		return true, "" // 拿不到用量就不拦：宁可让上传路径兜底，也不误伤正常上传
	}

	if limitMB > 0 {
		if over, msg := quotaExceeded(limitMB, rt.DiskUsed, size); over {
			return false, msg
		}
	}
	// 节点剩余空间：与 Daemon 侧同一套口径（写完后至少留 uploadFreeMargin）。
	// 这里提前告知，省得传到一半才失败。
	if rt.DiskFree > 0 && rt.DiskFree-size < uploadFreeMargin {
		return false, "节点磁盘剩余空间不足：本次需要 " + humanBytes(size) +
			"，当前可用 " + humanBytes(rt.DiskFree) +
			"（平台会为节点预留 " + humanBytes(uploadFreeMargin) + " 余量）"
	}
	return true, ""
}

// uploadFreeMargin 与 Daemon 侧保持一致的磁盘余量要求（见 grpcapi.freeBytes 的注释）。
//
// 两处写同一个数字有漂移风险，但把它放到 proto/配置里为这一个常量加一层不值得；
// 这里用注释互相指认，改一处时另一处也会被搜到。
const uploadFreeMargin = 1 << 30

// drainBeforeReject 提前拒绝时最多读掉多少请求体。
//
// 取 32MB 是个折中：足以覆盖常见被拒场景（几 MB 的包），又不至于让"拒绝"本身
// 变成一次几百 MB 的全量传输。超过这个量就放弃读取、直接关连接 ——
// 那种情况界面已经通过 /upload-check 预检过，正常用户不会走到。
const drainBeforeReject = 32 << 20

// drainBody 把剩余的请求体读掉（最多 limit 字节）之后再做错误响应。
//
// 为什么需要这一步：服务端在客户端**还在发送数据**时直接关连接，客户端看到的
// 是 `connection reset by peer`，而不是我们精心写好的那句 413 提示 ——
// 实测如此（20MB 请求被拒时 python 与 wget 都拿到 RST 而非响应体）。
// 先把 body 读掉，客户端就能正常收完响应再关连接。
//
// 注意别把它写成"无条件读完"：那样一个被拒的 256MB 上传会白白传完 256MB。
func drainBody(r *http.Request, limit int64) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, limit))
}

// uploadStreamErr 把"流中途失败"翻译成一句用户能看懂的话。
//
// 流式 RPC 的一个反直觉之处：服务端带着错误状态提前返回时，客户端的下一次
// Send 只会拿到 **io.EOF** —— 真正的原因藏在服务端状态里，要用 CloseAndRecv 取回。
// 直接用 sendErr 的文本，用户看到的是"上传失败：EOF"，既没有信息量，
// 又会把人引向"网络问题"这个错误方向（实测脚本与界面都会这样显示）。
//
// 只在 Send 返回 io.EOF 时去取状态：其它情况（连接断了等）CloseAndRecv 也拿不到
// 更有用的话，反而可能多等一轮。
func uploadStreamErr(stream pb.DaemonService_UploadFileClient, sendErr error) string {
	if errors.Is(sendErr, io.EOF) {
		if _, err := stream.CloseAndRecv(); err != nil {
			return uploadErrText(err)
		}
		// 服务端其实成功收完了（极少数竞态）：别谎报失败
		return "上传中断：服务端已提前结束连接，请重试"
	}
	return uploadErrText(sendErr)
}

// uploadErrText 把 gRPC 错误压成一句能给用户看的话。
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
