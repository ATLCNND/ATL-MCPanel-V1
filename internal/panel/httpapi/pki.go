package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
)

// 节点证书有效期
const nodeCertTTL = 3 * 365 * 24 * time.Hour

// handleGetNodeCert GET /api/nodes/{id}/cert
//
// 返回该节点接入所需的 mTLS 材料（CA 证书 + 客户端证书 + 私钥）。
// 每次调用都会重新签发（即轮换），旧证书在过期前仍然有效。
func (s *Server) handleGetNodeCert(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if s.ca == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板尚未初始化 PKI")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}

	var name string
	if err := s.db.QueryRow(`SELECT name FROM nodes WHERE id = ?`, id).Scan(&name); err != nil {
		writeErr(w, http.StatusNotFound, "节点不存在")
		return
	}

	s.writeNodeCert(w, r, name, "按节点签发")
}

// handleIssueNodeCert POST /api/nodes/cert
// 直接按名称签发节点证书（无需先登记节点），便于新节点接入。
func (s *Server) handleIssueNodeCert(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if s.ca == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板尚未初始化 PKI")
		return
	}
	var req struct {
		NodeName string `json:"node_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeName == "" {
		writeErr(w, http.StatusBadRequest, "node_name 必填")
		return
	}
	s.writeNodeCert(w, r, req.NodeName, "按名称签发")
}

// writeNodeCert 签发节点证书并返回全部接入材料。
func (s *Server) writeNodeCert(w http.ResponseWriter, r *http.Request, nodeName, detail string) {
	certPEM, keyPEM, err := s.ca.IssueClientCertWithTTL(nodeName, nodeCertTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发节点证书失败: "+err.Error())
		return
	}

	s.audit(r, "issue_node_cert", nodeName,
		fmt.Sprintf("%s，CN=%s，有效期 %d 天", detail, nodeName, int(nodeCertTTL.Hours()/24)))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"node_name":    nodeName,
		"ca_cert":      string(s.ca.CertPEM),
		"client_cert":  string(certPEM),
		"client_key":   string(keyPEM),
		"server_name":  pki.ServerName,
		"expires_days": int(nodeCertTTL.Hours() / 24),
		"message": "将 ca_cert/client_cert/client_key 分别写入节点上的 ca_file/cert_file/key_file，" +
			"并设置 daemon.tls: true 后重启 Daemon",
	})
}

// handleGetPKIInfo GET /api/pki
//
// 返回 PKI 概览（仅管理员），便于运维确认 mTLS 状态。
func (s *Server) handleGetPKIInfo(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if s.ca == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"initialized": false})
		return
	}
	certPath, keyPath := s.ca.FilePaths()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"initialized":  true,
		"subject":      s.ca.Cert.Subject.CommonName,
		"not_after":    s.ca.Cert.NotAfter.Format(time.RFC3339),
		"server_name":  pki.ServerName,
		"grpc_mtls":    s.grpcMTLS,
		"ca_cert_path": certPath,
		"ca_key_path":  keyPath,
		"ca_cert":      string(s.ca.CertPEM),
	})
}
