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
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		writeErr(w, http.StatusUnauthorized, "续期必须通过 mTLS 提供当前证书")
		return
	}
	cur := r.TLS.PeerCertificates[0]
	uid := certNodeUID(cur)
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "当前证书里没有节点身份（node_uid），请重新走一次凭证交换")
		return
	}
	if time.Now().After(cur.NotAfter) {
		writeErr(w, http.StatusUnauthorized, "当前证书已过期，请重新走一次凭证交换")
		return
	}

	var revoked int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM node_enrollments WHERE node_uid = ? AND revoked_at IS NOT NULL`, uid).Scan(&revoked)
	if revoked > 0 {
		writeErr(w, http.StatusForbidden, "该节点已被撤销，无法续期")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, enrollBodyLimit)
	var req struct {
		CSR string `json:"csr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CSR == "" {
		writeErr(w, http.StatusBadRequest, "缺少 csr")
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

	name := cur.Subject.CommonName
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
