package httpapi

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"time"
)

// 证书续期（M3）。
//
// 与首次接入的区别：**不要求 token** —— 凭据就是那张仍然有效的证书。
// 校验三件事（任何一条不过就拒绝，不宽容）：
//   1. 请求确实带着客户端证书（否则说明没走 mTLS，不能凭空续期）；
//   2. 证书未过期，且能从 URI SAN 取出 node_uid；
//   3. 该 node_uid 没有未处理的撤销，且新 CSR 的公钥指纹与首次登记一致
//      （防止「拿一张旧证书 + 别人的公钥」换新证书）。
func (s *Server) handleEnrollRenew(w http.ResponseWriter, r *http.Request) {
	if s.ca == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板未启用 PKI，无法续期")
		return
	}
	// 认证方式：**不看连接层证书**（面板的 HTTP 监听并不请求客户端证书，靠它永远拿不到）。
	// 证明「你就是那个节点」靠两件事：
	//   ① 请求里声明的 node_uid 在 panel 有**未撤销**的 enrollment 记录；
	//   ② 新 CSR 的公钥指纹与该记录里登记的一致 —— 而 CSR 自带自签名校验（在 SignCSR 里），
	//      自签名有效即证明对方**持有对应私钥**。
	// 于是不需要 mTLS 也具备同样的安全属性：拿不到原私钥就换不出新证书。
	var req struct {
		NodeUID string `json:"node_uid"`
		CSR     string `json:"csr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	uid := strings.TrimSpace(req.NodeUID)
	if uid == "" || req.CSR == "" {
		writeErr(w, http.StatusBadRequest, "node_uid 与 csr 均为必填")
		return
	}
	var revoked int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM node_enrollments WHERE node_uid = ? AND revoked_at IS NOT NULL`, uid).Scan(&revoked)
	if revoked > 0 {
		writeErr(w, http.StatusForbidden, "该节点已被撤销，无法续期")
		return
	}

	// 用节点行上的名字作为证书 CN（保持显示名一致）
	name := ""
	_ = s.db.QueryRow(`SELECT name FROM nodes WHERE node_uid = ?`, uid).Scan(&name)
	if name == "" {
		writeErr(w, http.StatusForbidden, "找不到该节点记录，请重新走一次凭证交换")
		return
	}
	// 公钥指纹必须与首次登记一致：续期是「同一把钥匙换张新证」，不是换钥匙
	newFP := csrPubkeyFingerprint(req.CSR)
	if newFP == "" {
		writeErr(w, http.StatusBadRequest, "CSR 无法解析")
		return
	}
	var lastFP string
	_ = s.db.QueryRow(`SELECT COALESCE(pubkey_fp,'') FROM node_enrollments
		WHERE node_uid = ? ORDER BY id DESC LIMIT 1`, uid).Scan(&lastFP)
	if lastFP != "" && lastFP != newFP {
		writeErr(w, http.StatusForbidden, "续期必须沿用首次登记的密钥（公钥指纹不一致）")
		return
	}

	certPEM, err := s.ca.SignCSR([]byte(req.CSR), name, uid, enrollCertTTL)
	if err != nil {
		s.auditAs(0, r, "enroll_renew_failed", uid, err.Error())
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	expires := time.Now().Add(enrollCertTTL)
	_, _ = s.db.Exec(`INSERT INTO node_enrollments (node_uid, cert_fp, pubkey_fp, expires_at)
		VALUES (?, ?, ?, ?)`, uid, sha256Hex(certPEM), newFP, expires.Unix())
	s.auditAs(0, r, "enroll_renew", uid, "name="+name)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cert":        string(certPEM),
		"ca":          string(s.ca.CertPEM),
		"node_uid":    uid,
		"expires_at":  expires.UTC().Format(time.RFC3339),
		"renew_after": time.Now().Add(enrollCertTTL - 7*24*time.Hour).UTC().Format(time.RFC3339),
	})
}

// certNodeUID 取证书 URI SAN 里的 node_uid（与 pki.CertNodeUID 同一约定）。
func certNodeUID(c *x509.Certificate) string {
	if c == nil {
		return ""
	}
	for _, u := range c.URIs {
		if u != nil && u.Scheme == "atlmcpanel" && u.Host == "node" {
			return strings.TrimPrefix(u.Path, "/")
		}
	}
	return ""
}

// csrPubkeyFingerprint 计算 CSR 里公钥的指纹（DER 的 sha256），用于「必须同一把钥匙」。
func csrPubkeyFingerprint(csrPEM string) string {
	blk, _ := pem.Decode([]byte(csrPEM))
	if blk == nil {
		return ""
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil || csr.PublicKey == nil {
		return ""
	}
	der, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
