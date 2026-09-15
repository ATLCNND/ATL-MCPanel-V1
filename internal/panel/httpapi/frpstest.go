package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 线路自检的 HTTP 入口（仅总管理员）。
//
//	POST /api/frps/test      body = 表单里那几个字段（**保存之前**就能测）
//	POST /api/frps/{id}/test 用已保存的线路配置测
//
// 自检**在节点上跑**（Daemon 的 TestFrps）：真正跑 frpc 的是节点，
// 面板能连通 ≠ 节点能连通 —— 只从面板测会得出"线路没问题"的错误结论。
func (s *Server) handleTestFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	req, ok := s.readFrpsTestInput(w, r)
	if !ok {
		return
	}
	s.runFrpsTest(w, r, req)
}

// handleTestFrpsSaved POST /api/frps/{id}/test —— 用库里已保存的配置测。
func (s *Server) handleTestFrpsSaved(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "线路 id 无效")
		return
	}
	var host, token string
	var bindPort, portStart, portEnd int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token, port_start, port_end FROM frps_servers WHERE id = ?`, id).
		Scan(&host, &bindPort, &token, &portStart, &portEnd); err != nil {
		writeErr(w, http.StatusNotFound, "线路不存在")
		return
	}
	s.runFrpsTest(w, r, frpsTestInput{
		Host: host, BindPort: bindPort, Token: token,
		PortStart: portStart, PortEnd: portEnd,
	})
}

// frpsTestInput 自检需要的字段。
type frpsTestInput struct {
	Host      string `json:"host"`
	BindPort  int32  `json:"bind_port"`
	Token     string `json:"token"`
	PortStart int32  `json:"port_start"`
	PortEnd   int32  `json:"port_end"`
	// NodeID 指定在哪个节点上测；0 = 自动挑第一个可用节点
	NodeID int64 `json:"node_id"`
}

// readFrpsTestInput 解析并校验自检输入。
func (s *Server) readFrpsTestInput(w http.ResponseWriter, r *http.Request) (frpsTestInput, bool) {
	var in frpsTestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return in, false
	}
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" {
		writeErr(w, http.StatusBadRequest, "线路地址不能为空")
		return in, false
	}
	if in.BindPort <= 0 {
		in.BindPort = 7000
	}
	if in.PortStart <= 0 {
		in.PortStart = 25565
	}
	if in.PortEnd < in.PortStart {
		in.PortEnd = in.PortStart + 100
	}
	return in, true
}

// runFrpsTest 选节点 → 调 Daemon 自检 → 把结论整理成 JSON。
func (s *Server) runFrpsTest(w http.ResponseWriter, r *http.Request, in frpsTestInput) {
	nodeID := in.NodeID
	if nodeID == 0 {
		// 自动挑第一个节点：线路是全局的，任何一个装了 frpc 的节点都能验证它
		// （返回体里会写明是在哪个节点上测的，避免"换台机器结果不同"时无从对照）
		if err := s.db.QueryRow(`SELECT id FROM nodes ORDER BY id LIMIT 1`).Scan(&nodeID); err != nil {
			writeErr(w, http.StatusBadRequest, "还没有登记任何节点，无法自检（穿透是节点上的 frpc 在跑）")
			return
		}
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "节点连接失败："+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp, err := cli.TestFrps(ctx, &pb.TestFrpsRequest{
		Host:      in.Host,
		BindPort:  in.BindPort,
		Token:     in.Token,
		PortStart: in.PortStart,
		PortEnd:   in.PortEnd,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "自检请求失败："+err.Error())
		return
	}

	var nodeName string
	_ = s.db.QueryRow(`SELECT name FROM nodes WHERE id = ?`, nodeID).Scan(&nodeName)

	s.audit(r, "test_frps", in.Host,
		"节点="+nodeName+" 结论="+conclusionOf(resp))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        resp.Success,
		"error":          resp.Error,
		"dial_ok":        resp.DialOk,
		"dial_ms":        resp.DialMs,
		"dial_error":     resp.DialError,
		"register_ok":    resp.RegisterOk,
		"used_port":      resp.UsedPort,
		"register_error": resp.RegisterError,
		"log_tail":       resp.LogTail,
		"node_id":        nodeID,
		"node_name":      nodeName,
		"address":        in.Host + ":" + strconv.Itoa(int(in.BindPort)),
	})
}

// conclusionOf 给审计日志用的一句话结论。
func conclusionOf(r *pb.TestFrpsResponse) string {
	switch {
	case r.Success:
		return "通过"
	case !r.DialOk:
		return "TCP 不可达"
	default:
		return "注册失败"
	}
}
