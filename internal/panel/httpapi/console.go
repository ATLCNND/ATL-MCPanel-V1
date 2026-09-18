package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ATLCNND/ATL-MCPanel/internal/consolefmt"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 开发阶段允许所有来源
}

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
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 先发送 ATTACH
	if err := stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_ATTACH, InstanceId: instanceID}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 升级 WS
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

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

	// goroutine 1: gRPC -> WS（Daemon 输出转发给前端）
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			frame, err := stream.Recv()
			if err != nil {
				send(consoleFrame{Type: "notice", Level: consolefmt.LevelWarn, Data: "[连接关闭]\n"})
				return
			}
			switch frame.Type {
			case pb.ConsoleFrame_OUTPUT:
				// 级别由 Daemon 涂的行首配色反推（见 internal/consolefmt）：
				// 那边已经判过一次，不在这里重判 —— 一份规则，两处使用。
				send(consoleFrame{
					Type:  "line",
					Level: consolefmt.LevelOfDecorated(frame.Data),
					Data:  frame.Data,
				})
			case pb.ConsoleFrame_STATUS:
				send(consoleFrame{
					Type:  "status",
					Level: consolefmt.LevelInfo,
					Data:  "[状态] " + frame.Data + "\n",
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
