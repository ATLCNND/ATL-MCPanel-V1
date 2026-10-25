package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ATLCNND/ATL-MCPanel/internal/consolefmt"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 开发阶段允许所有来源
}

// maxConsoleAttachParam 浏览器 `?spam=` 参数的长度上限（防御性；Daemon 侧另有
// 规则条数、单条长度与回放行数的上限）。超限就忽略，回放退回默认行为，不会报错。
//
// 参数名沿用 `spam`（历史原因），但它承载的是**整份附加参数**：
//
//	{ "spam": { "rules": [...] }, "history": 1000 }
//
// 面板对内容一律不解释、只做长度校验后原样转发 —— 判定标准只有一份，
// 必须在前端与 Daemon 之间直传，多一层解释就多一处走偏的机会。
const maxConsoleAttachParam = 4096

// consoleFrame 控制台流发给浏览器的消息。
//
// 为什么是 JSON 而不是直接把 Daemon 的输出文本写进去：
// 前端要提供"全部 / 信息 / 警告 / 错误"的筛选，就必须知道**每一行是什么级别**，
// 而级别只在 Daemon 那边判定过（它按行涂了底色）。所以转发时把级别一起带上，
// 前端可以本地即时切换筛选、不必重连、也不必自己再写一套识别规则
// （那会是第二份实现，迟早跟 Daemon 的规则走偏）。
//
// data 里保留原始转义序列：xterm 要靠它渲染颜色和底色。
type consoleFrame struct {
	Type string `json:"type"` // line / status / notice
	// Level info / warn / error（取值见 internal/consolefmt）
	Level string `json:"level"`
	Data  string `json:"data"`
	// Seq 这条连接的帧序号（从 1 开始，只在 line/status 上递增）。
	//
	// 为什么需要（B4 排查，2026-10-21）：现在帧里**没有任何序号**，于是
	// "回放快照"与"实时直播"在浏览器侧长得一模一样 —— 出现"连上后先到的
	// 是上一次启动的历史行"这种时间线错乱时无法区分，也说不清"到底收到了
	// 几帧、第几帧开始断的"。加了序号之后：
	//   · 前端可以识别"快照段 / 直播段"，也能按序插回（F14 双缓冲的前提）；
	//   · 排查时只要看最后一个 seq，就知道断在哪一帧。
	// 老前端会忽略这个字段（JSON 多一个键不影响解析），所以可以先行上线。
	Seq uint64 `json:"seq,omitempty"`
}

// consoleConnSeq 给每条控制台连接一个自增编号（只用于日志串联）。
var consoleConnSeq atomic.Uint64

// consoleConnID 生成"实例 + 序号"的短标识。
//
// 为什么要它：控制台是**每次进页签就重连一次**的界面，一个实例上短时间内会有
// 多条连接。没有标识时，多行日志只能靠时间戳猜哪几行属于同一次连接 ——
// B4 这种"偶发、要按连接对齐时间线"的问题会因此完全查不动。
func consoleConnID(instanceID string) string {
	return instanceID + "#" + strconv.FormatUint(consoleConnSeq.Add(1), 10)
}

// handleConsole WS 控制台代理：
//
//	前端 WS <-> Panel <-> gRPC console stream <-> Daemon
//
// 鉴权：浏览器 WS 无法自定义 header，故通过 query 参数 ?token= 传 JWT。
// 权限：viewer 可只读观察，collab 及以上可发送命令。
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")

	// 解析 token（query 参数）
	token := r.URL.Query().Get("token")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "缺少 token")
		return
	}
	claims, err := s.auth.ParseToken(token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "令牌无效或已过期")
		return
	}

	// ⚠️ ParseToken 只验签名与有效期，**不验 token_version**。
	//
	// 那条校验原先只在 requireAuth 中间件里做（见 authUser），而控制台走的是
	// WebSocket、绕过了中间件 —— 于是「改密码 / 管理员撤销令牌」之后，**旧令牌仍能连控制台**。
	// 验收清单 B-98 的三条件测试把这个洞测出来了：推进 token_version 后 WSS 依然 101。
	//
	// 这里补上同样的检查：用户状态必须 active，且令牌世代号与库里一致。
	var role, state string
	var tv int64
	if err := s.db.QueryRow(
		`SELECT role, COALESCE(status,''), COALESCE(token_version,0) FROM users WHERE id = ?`,
		claims.UserID).Scan(&role, &state, &tv); err != nil {
		writeErr(w, http.StatusUnauthorized, "登录状态已失效")
		return
	}
	if (state != "" && state != "active") || tv != claims.TokenVersion {
		writeErr(w, http.StatusUnauthorized, "登录状态已失效，请重新登录")
		return
	}

	// 权限检查：至少 viewer 才能观察
	level, ok := s.instanceLevel(claims.UserID, claims.Role, instanceID)
	if !ok {
		writeErr(w, http.StatusForbidden, "无权访问该实例")
		return
	}
	canSend := levelAtLeast(level, LevelCollab)

	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 建立 gRPC console 双向流。用可取消的 context：收尾时要靠它把
	// Recv 叫醒（否则转发协程会一直挂在 Recv 上等 Daemon 输出）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := cli.Console(ctx)
	if err != nil {
		slog.Warn("控制台：建立 gRPC 流失败", "instance", instanceID, "node", nodeID, "error", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// ---- B4 排查用的观测点（2026-10-21）----
	//
	// 现象：WS 建起来了，但一条 line 帧都收不到（连服务端每分钟的心跳都没有），
	// 而 Daemon 侧**没有**「控制台附加」记录 —— 也就是面板侧的订阅没落到 Daemon。
	// 在此之前这条链路在面板侧**一行日志都没有**（只有 Daemon 侧有），
	// 排查只能靠猜。所以把关键节点都打出来，并给每条连接一个 conn 标识，
	// 便于把同一连接的多行日志串起来：
	//   ① 请求到达（谁 / 哪个实例 / 能不能发命令）
	//   ② gRPC 流已建立
	//   ③ ATTACH 已发送（带 payload 大小）—— 与 Daemon 的「控制台附加」对时间
	//   ④ WS 升级完成
	//   ⑤ **收到 Daemon 的第一个帧**（最关键：区分"Daemon 没回"与"回了但前端没收到"）
	//   ⑥ 收尾：总帧数、存活时长、结束原因
	//
	// 判据怎么用（写在这里，免得下次接手重新推）：
	//   · 有 ③ 无 Daemon 的「控制台附加」→ 问题在 gRPC 传输/路由这一层；
	//   · Daemon 有「控制台附加」但无 ⑤ → 问题在 Daemon 回帧或面板 Recv；
	//   · 有 ⑤ 但浏览器界面空白 → 问题在面板→浏览器这段（检查 seq 与 WS 状态）。
	connID := consoleConnID(instanceID)
	startedAt := time.Now()
	slog.Info("控制台：请求受理", "conn", connID, "instance", instanceID,
		"user", claims.UserID, "node", nodeID, "can_send", canSend)
	defer func() {
		// ⑥ 收尾：配合上面"收到首帧/接收结束"两行，可以把一次连接的完整时间线拼出来
		slog.Info("控制台：会话结束", "conn", connID, "instance", instanceID,
			"elapsed_ms", time.Since(startedAt).Milliseconds())
	}()
	slog.Info("控制台：gRPC 流已建立", "conn", connID, "instance", instanceID, "node", nodeID)

	// 先发送 ATTACH
	attach := &pb.ConsoleFrame{Type: pb.ConsoleFrame_ATTACH, InstanceId: instanceID}
	// 浏览器可以在 URL 上带 `spam=<json>`：它是**整份附加参数** ——
	// 既包含当前生效的刷屏屏蔽规则（预制里开着的 + 用户自定义正则），
	// 也包含设置里的「保留行数」（history）。前者决定回放窗口**按什么计数**
	// （被屏蔽的心跳不占额度），后者决定**回放多深**（2026-10-21 之前写死 500，
	// 于是前端滑块改了也不生效）。
	//
	// 面板**不解释**这些内容，只做长度上限校验后原样转发：判定标准只有一份，
	// 必须在前端与 Daemon 之间直传，多一层解释就会多一处走偏的机会。
	// 长度上限是防御性的：这个参数来自 URL，不能让它变成内存/CPU 放大器
	// （Daemon 侧还有规则条数、单条长度与回放行数的上限）。
	if raw := r.URL.Query().Get("spam"); raw != "" {
		if len(raw) <= maxConsoleAttachParam {
			attach.Data = raw
		} else {
			slog.Warn("控制台附加参数过长，已忽略（回放退回默认行为）",
				"instance", instanceID, "bytes", len(raw), "limit", maxConsoleAttachParam)
		}
	}
	if err := stream.Send(attach); err != nil {
		slog.Warn("控制台：发送 ATTACH 失败", "conn", connID, "instance", instanceID, "error", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// ③ 这一行要能与 Daemon 的「控制台附加」对上时间：两者都出现，说明订阅确实到了。
	slog.Info("控制台：ATTACH 已发送", "conn", connID, "instance", instanceID,
		"payload_bytes", len(attach.Data))

	// 升级 WS
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("控制台：WS 升级失败", "conn", connID, "instance", instanceID, "error", err)
		return
	}
	defer ws.Close()
	slog.Info("控制台：WS 已升级", "conn", connID, "instance", instanceID)

	// 写锁：下面的读命令协程与输出转发协程**都会**往同一条 WS 写。
	// gorilla/websocket 只允许一个并发写者，两个协程直接写会 panic
	// （concurrent write to websocket connection）或交错出坏帧 ——
	// 触发条件正是"用户在没有权限的情况下敲了条命令、同时服务端在刷屏"。
	var wmu sync.Mutex
	send := func(f consoleFrame) {
		b, err := json.Marshal(f)
		if err != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		// 写超时：客户端不读（页签被挂起、网络卡住）时，WriteMessage 会一直
		// 堵在 TCP 发送缓冲上，并且卡着 wmu 不放。没有这个期限，
		// 收尾时的 closeWS 就会跟着一起挂住。
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = ws.WriteMessage(websocket.TextMessage, b)
	}
	// closeWS 同样在写锁里关：避免与正在进行的写并发（net.Conn 的 Close
	// 与 Write 并发时行为不确定，锁内串行化最省心）。
	closeWS := func() {
		wmu.Lock()
		defer wmu.Unlock()
		_ = ws.Close()
	}

	done := make(chan struct{}, 2)

	// 帧序号与统计：seq 随每条已转发的帧递增（B4 排查与 F14 双缓冲共用，见 consoleFrame.Seq）。
	var seq atomic.Uint64
	var received atomic.Int64

	// goroutine 1: gRPC -> WS（Daemon 输出转发给前端）
	go func() {
		defer func() { done <- struct{}{} }()
		started := time.Now()
		for {
			frame, err := stream.Recv()
			if err != nil {
				// ⑤ 的反面：一条帧都没收到就结束了 —— 这正是 B4 的形态。
				// 带上网耗与错误，便于和 Daemon 侧「控制台附加」对时间线。
				slog.Warn("控制台：gRPC 接收结束", "conn", connID, "instance", instanceID,
					"frames", received.Load(), "after_ms", time.Since(started).Milliseconds(), "error", err)
				send(consoleFrame{Type: "notice", Level: consolefmt.LevelWarn, Data: "[连接关闭]\n"})
				return
			}
			if received.Add(1) == 1 {
				// ⑤ **最关键的一行**：Daemon 确实回了帧。
				// 若浏览器仍空白而这里有记录，问题就在面板→浏览器这段。
				slog.Info("控制台：收到 Daemon 首帧", "conn", connID, "instance", instanceID,
					"type", frame.Type.String(), "after_ms", time.Since(started).Milliseconds())
			}
			switch frame.Type {
			case pb.ConsoleFrame_OUTPUT:
				// 级别由 Daemon 涂的行首配色反推（见 internal/consolefmt）：
				// 那边已经判过一次，不在这里重判 —— 一份规则，两处使用。
				send(consoleFrame{
					Type:  "line",
					Level: consolefmt.LevelOfDecorated(frame.Data),
					Data:  frame.Data,
					Seq:   seq.Add(1),
				})
			case pb.ConsoleFrame_STATUS:
				send(consoleFrame{
					Type:  "status",
					Level: consolefmt.LevelInfo,
					Data:  "[状态] " + frame.Data + "\n",
					Seq:   seq.Add(1),
				})
			}
		}
	}()

	// goroutine 2: WS -> gRPC（前端输入的命令发给 Daemon）
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				_ = stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_DETACH, InstanceId: instanceID})
				return
			}
			// viewer 只读：忽略命令
			if !canSend {
				send(consoleFrame{
					Type:  "notice",
					Level: consolefmt.LevelWarn,
					Data:  "\x1b[90m[只读模式：无发送命令权限]\x1b[0m\n",
				})
				continue
			}
			_ = stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_COMMAND, InstanceId: instanceID, Data: string(msg)})
		}
	}()

	// 等任一方向结束，然后收尾。
	//
	// 这里**不能**用 select{}：那会真的永久阻塞 —— 两条协程都退出之后，
	// 处理器协程还挂着，WS 也不会被关（defer 永远不执行）。控制台是
	// "每次进页签、每次刷新页面都会重连"的界面，于是每开一次就漏一个
	// 协程 + 一条连接 + 一条 gRPC 流，长时间开着面板会持续涨。
	<-done
	cancel()  // 叫醒挂在 Recv 上的转发协程
	closeWS() // 叫醒挂在 ReadMessage 上的读命令协程
	// 不等第二条：它只可能因为上面的 cancel/close 立刻返回，
	// 而它退出前若还要写一次，也只会写失败（错误已忽略），不会 panic。
}
