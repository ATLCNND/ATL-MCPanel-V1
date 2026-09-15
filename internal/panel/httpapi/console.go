package httpapi

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 开发阶段允许所有来源
}

// handleConsole WS 控制台代理：
//  前端 WS <-> Panel <-> gRPC console stream <-> Daemon
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

	// 建立 gRPC console 双向流
	stream, err := cli.Console(context.Background())
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

	// goroutine 1: gRPC -> WS（Daemon 输出转发给前端）
	go func() {
		for {
			frame, err := stream.Recv()
			if err != nil {
				_ = ws.WriteMessage(websocket.TextMessage, []byte("[连接关闭]\n"))
				return
			}
			if frame.Type == pb.ConsoleFrame_OUTPUT {
				_ = ws.WriteMessage(websocket.TextMessage, []byte(frame.Data))
			} else if frame.Type == pb.ConsoleFrame_STATUS {
				_ = ws.WriteMessage(websocket.TextMessage, []byte("[状态] "+frame.Data+"\n"))
			}
		}
	}()

	// goroutine 2: WS -> gRPC（前端输入的命令发给 Daemon）
	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				_ = stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_DETACH, InstanceId: instanceID})
				return
			}
			// viewer 只读：忽略命令
			if !canSend {
				_ = ws.WriteMessage(websocket.TextMessage, []byte("\x1b[90m[只读模式：无发送命令权限]\x1b[0m\n"))
				continue
			}
			_ = stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_COMMAND, InstanceId: instanceID, Data: string(msg)})
		}
	}()

	// 阻塞直到 WS 关闭
	select {}
}