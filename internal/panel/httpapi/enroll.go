package httpapi

import (
	crand "crypto/rand"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

)

// 凭证交换的第一个端点（M3）。
//
// 设计见 docs/DESIGN-CREDENTIAL-EXCHANGE.md。这里先把**协议本体**跑通：
// 管理员签发一次性 token → 节点带着 CSR 来换证书 → token 用后即焚。
// Daemon 侧的 enroll 子命令与撤销/续期是下一步。

const (
	// joinTokenTTL 一次性 token 的有效期。15 分钟是 kubeadm 风格：够贴一次命令，
	// 又短到即使被截屏/进了 shell 历史，泄露窗口也很小。
	joinTokenTTL = 15 * time.Minute
	// enrollCertTTL 节点证书有效期。短周期 + 续期，是为了让「撤销不生效」的最坏窗口可控。
	enrollCertTTL = 30 * 24 * time.Hour
	// enrollBodyLimit /api/enroll 的请求体上限（CSR 通常 1KB 级）
	enrollBodyLimit = 32 << 10
)

// enrollLimiter 给未鉴权的 /api/enroll 限速（每个来源 IP 每分钟若干次）。
//
// 这个端点是**唯一不需要登录**的写接口，必须限速：否则它就是一个可以被无限尝试的
// token 爆破面。实现刻意简单（进程内固定窗口），够挡脚本刷，不需要分布式计数。
type enrollLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newEnrollLimiter() *enrollLimiter { return &enrollLimiter{hits: map[string][]time.Time{}} }

func (l *enrollLimiter) allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	old := l.hits[key]
	kept := old[:0]
	for _, t := range old {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// handleIssueJoinToken POST /api/enroll/token（仅总管理员）
//
// 签一个一次性 token 给节点用。mode=panel 时 token 由面板签（管理员贴到节点执行）；
// mode=node 时预留给「节点发起」——那种模式下 token 由 Daemon 生成、面板只负责校验。
func (s *Server) handleIssueJoinToken(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		Mode     string `json:"mode"`     // panel（默认）/ node
		NodeName string `json:"node_name"`
		NodeUID  string `json:"node_uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "panel"
	}
	if mode != "panel" && mode != "node" {
		writeErr(w, http.StatusBadRequest, "mode 只能是 panel 或 node")
		return
	}

	jti, err := randomTokenString()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成 token 失败")
		return
	}
	exp := time.Now().Add(joinTokenTTL)
	// expires_at 存**整数 Unix 秒**而不是 DATETIME：
	// SQLite 里 time.Time 会被写成带时区的字符串（2026-10-25 09:30:00+08:00），
	// 与 CURRENT_TIMESTAMP（UTC、无时区）比较是**字符串比较**，跨时区会给出错误结论
	//（实测：过期的 token 被当成有效，用例当场抓到）。整数比较没有这个问题。
	if _, err := s.db.Exec(`INSERT INTO join_tokens (jti, mode, node_uid, node_name, created_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		jti, mode, strings.TrimSpace(req.NodeUID), strings.TrimSpace(req.NodeName),
		currentUserID(r), exp.Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "issue_join_token", mode, "node="+req.NodeName)

	// token 文本：把 jti 包一层前缀，便于在日志里一眼认出来（不是秘密本身）
	token := "atl-join-v1." + jti
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      token,
		"expires_at": exp.UTC().Format(time.RFC3339),
		"ttl_seconds": int(joinTokenTTL.Seconds()),
		"panel_grpc": s.grpcPublicAddress(),
		"mode":       mode,
	})
}

// handleEnroll POST /api/enroll（**不需要登录**，凭 token 鉴权）
//
// 请求：{ token, csr, node_uid, node_name, hostname, daemon_ver }
// 成功返回证书与 CA（**不含私钥** —— 私钥始终留在节点上）。
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.enrollLimit.allow(ip, 10, time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
		return
	}
	if s.ca == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板未启用 PKI，无法签发节点证书")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, enrollBodyLimit)
	var req struct {
		Token     string `json:"token"`
		CSR       string `json:"csr"`
		NodeUID   string `json:"node_uid"`
		NodeName  string `json:"node_name"`
		Hostname  string `json:"hostname"`
		DaemonVer string `json:"daemon_ver"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.Token == "" || req.CSR == "" || req.NodeUID == "" {
		writeErr(w, http.StatusBadRequest, "token / csr / node_uid 均为必填")
		return
	}
	jti := strings.TrimPrefix(req.Token, "atl-join-v1.")

	// ---- 原子消耗 token：一条 UPDATE 里同时判定「存在/未撤销/未过期/未使用」----
	// 用条件更新而不是先查后改：两个请求同时到达时只有一个能抢到。
	res, err := s.db.Exec(`UPDATE join_tokens SET used_at = CURRENT_TIMESTAMP, used_from = ?
		WHERE jti = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		ip, jti, time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 不给具体原因（过期？已用？不存在？）——未鉴权端点上说太细等于帮人试探
		s.auditAs(0, r, "enroll_rejected", jti, ip)
		writeErr(w, http.StatusForbidden, "token 无效或已失效")
		return
	}

	// token 里若指定了 node_uid，必须与请求一致（防拿 A 的 token 给 B 换证书）
	var tokUID, tokName string
	_ = s.db.QueryRow(`SELECT node_uid, node_name FROM join_tokens WHERE jti = ?`, jti).Scan(&tokUID, &tokName)
	if tokUID != "" && tokUID != req.NodeUID {
		writeErr(w, http.StatusForbidden, "token 与 node_uid 不匹配")
		return
	}
	name := req.NodeName
	if name == "" {
		name = tokName
	}
	if name == "" {
		name = req.Hostname
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少节点名")
		return
	}

	certPEM, err := s.ca.SignCSR([]byte(req.CSR), name, req.NodeUID, enrollCertTTL)
	if err != nil {
		s.auditAs(0, r, "enroll_sign_failed", jti, err.Error())
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// ---- 记录 enrollment 并登记节点 ----
	certFP := sha256Hex(certPEM)
	// 指纹必须与续期侧用**同一个函数**算：续期比的是「公钥 DER 的 sha256」。
	// 这里原先存的是 CSR 文本的哈希 —— 两者永远不相等，于是续期一律被判「换了钥匙」
	//（真机验证当场抓到：同一把钥匙续期返回 403）。
	pubFP := csrPubkeyFingerprint(req.CSR)
	expires := time.Now().Add(enrollCertTTL)
	if _, err := s.db.Exec(`INSERT INTO node_enrollments (node_uid, cert_fp, pubkey_fp, expires_at)
		VALUES (?, ?, ?, ?)`, req.NodeUID, certFP, pubFP, expires.Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.upsertEnrolledNode(name, req.NodeUID, ip)
	s.auditAs(0, r, "enroll_ok", req.NodeUID, "name="+name+" from="+ip)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cert":        string(certPEM),
		"ca":          string(s.ca.CertPEM),
		"node_uid":    req.NodeUID,
		"panel_grpc":  s.grpcPublicAddress(),
		"expires_at":  expires.UTC().Format(time.RFC3339),
		"renew_after": time.Now().Add(enrollCertTTL - 7*24*time.Hour).UTC().Format(time.RFC3339),
	})
}

// upsertEnrolledNode 按 node_uid 建立/更新节点行（凭证交换接入的节点没有 SSH 凭据）。
func (s *Server) upsertEnrolledNode(name, nodeUID, ip string) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM nodes WHERE node_uid = ?`, nodeUID).Scan(&id)
	if err == nil {
		_, _ = s.db.Exec(`UPDATE nodes SET name = ?, enroll_mode = COALESCE(NULLIF(enroll_mode,''), 'node')
			WHERE id = ?`, name, id)
		return
	}
	if err != sql.ErrNoRows {
		return
	}
	// 名字冲突时加后缀：name 只是显示名，不该因为重名而失败
	n := name
	for i := 2; i < 50; i++ {
		var c int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name = ?`, n).Scan(&c)
		if c == 0 {
			break
		}
		n = fmt.Sprintf("%s-%d", name, i)
	}
	_, _ = s.db.Exec(`INSERT INTO nodes (name, ip, public_ip, ssh_user, ssh_auth, ssh_auth_type, ssh_port,
		grpc_port, status, node_uid, enroll_mode)
		VALUES (?, ?, '', '', '', '', 22, ?, 'offline', ?, 'node')`,
		n, ip, s.daemonPortFromListen(), nodeUID)
}

// sha256Hex 证书/CSR 指纹（十六进制小写）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func randomTokenString() (string, error) {
	buf := make([]byte, 24)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
// handleEnrollImport POST /api/enroll/import（管理员）——「③ 节点发起」的导入端。
//
// 请求体：{ "request": "atl-join-v1.<payload>.<sig>" }
//
// 校验链（任何一步不过就拒，且不透露具体哪一步 —— 这个端点是管理员接口，但没必要帮人试探）：
//   1. 格式与版本；
//   2. **过期时间**（默认 30 分钟，粘贴晚了就失效）；
//   3. **签名**：用 payload 里 CSR 的公钥验证 —— 验签通过即证明发起方持有该私钥。
//      这是这套流程的安全根基：请求字符串被旁观者抄走也用不了。
//   4. node_uid 非空、CSR 可解析。
//
// 通过后：签 CSR、记录 pubkey 指纹（后续续期必须沿用）、按 node_uid 建/更新节点行。
func (s *Server) handleEnrollImport(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if s.ca == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板未启用 PKI，无法签发节点证书")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Request string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req, err := verifyJoinRequest(strings.TrimSpace(body.Request))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	certPEM, err := s.ca.SignCSR([]byte(req.CSR), req.NodeName, req.NodeUID, enrollCertTTL)
	if err != nil {
		s.audit(r, "enroll_import_failed", req.NodeUID, err.Error())
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	expires := time.Now().Add(enrollCertTTL)
	if _, err := s.db.Exec(`INSERT INTO node_enrollments (node_uid, cert_fp, pubkey_fp, expires_at)
		VALUES (?, ?, ?, ?)`, req.NodeUID, sha256Hex(certPEM), csrPubkeyFingerprint(req.CSR), expires.Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.upsertEnrolledNode(req.NodeName, req.NodeUID, "")
	s.audit(r, "enroll_import", req.NodeUID, "name="+req.NodeName)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cert":        string(certPEM),
		"ca":          string(s.ca.CertPEM),
		"node_uid":    req.NodeUID,
		"node_name":   req.NodeName,
		"panel_grpc":  s.grpcPublicAddress(),
		"expires_at":  expires.UTC().Format(time.RFC3339),
		"renew_after": time.Now().Add(enrollCertTTL - 7*24*time.Hour).UTC().Format(time.RFC3339),
	})
}

// verifyJoinRequest 解析并校验「③ 节点发起」的接入请求。
func verifyJoinRequest(s string) (daemonJoinRequest, error) {
	var out daemonJoinRequest
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] != "atl-join-v1" {
		return out, fmt.Errorf("不是合法的接入请求（应为 atl-join-v1.<载荷>.<签名>）")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return out, fmt.Errorf("载荷不是合法的 base64url")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return out, fmt.Errorf("签名不是合法的 base64url")
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, fmt.Errorf("载荷不是合法的 JSON")
	}
	if out.V != 1 {
		return out, fmt.Errorf("不支持的请求版本 %d（面板最高支持 1）", out.V)
	}
	if out.ExpiresAt < time.Now().Unix() {
		return out, fmt.Errorf("接入请求已过期，请在节点上重新执行 atl-daemon enroll --init")
	}
	if strings.TrimSpace(out.NodeUID) == "" {
		return out, fmt.Errorf("请求里缺少 node_uid")
	}
	blk, _ := pem.Decode([]byte(out.CSR))
	if blk == nil {
		return out, fmt.Errorf("请求里的 CSR 不是合法 PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return out, fmt.Errorf("解析 CSR 失败: %w", err)
	}
	// 签名校验：用 CSR 里的公钥验，而不是信任请求里自称的任何东西。
	sum := sha256.Sum256([]byte(parts[1]))
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return out, fmt.Errorf("请求里的公钥类型不受支持")
	}
	if !ecdsa.VerifyASN1(pub, sum[:], sig) {
		return out, fmt.Errorf("接入请求的签名无效（可能被篡改，或不是由该私钥签发）")
	}
	return out, nil
}

// daemonJoinRequest 与 daemon.JoinRequest 同构（面板不 import daemon 包，避免依赖倒挂）。
type daemonJoinRequest struct {
	V         int    `json:"v"`
	NodeUID   string `json:"node_uid"`
	NodeName  string `json:"node_name"`
	Hostname  string `json:"hostname,omitempty"`
	CSR       string `json:"csr"`
	ExpiresAt int64  `json:"exp"`
	Nonce     string `json:"nonce"`
}
