package httpapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 面板自签证书（M5 / F7）。
//
// 为什么要有它：给面板配 HTTPS 的前提是「有一张证书」。多数人手上没有正式证书，
// 而自签证书能立刻解决一个具体问题 —— 控制台 WebSocket 在 HTTPS 下可用
//（自签会让浏览器警告，点过「继续访问」之后 WS 是能用的）。
//
// 生成的文件落在 <dataDir>/certs/panel.crt|key，响应里会告诉用户去 config.yaml 引用。
func (s *Server) handleGenerateSelfSigned(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		Hosts []string `json:"hosts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hosts := normalizeTLSHosts(req.Hosts)
	if len(hosts) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少提供一个域名或 IP")
		return
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{s.displayName()}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dir := s.certsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建证书目录失败: "+err.Error())
		return
	}
	certPath := filepath.Join(dir, "panel.crt")
	keyPath := filepath.Join(dir, "panel.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "generate_self_signed", hosts[0], strings.Join(hosts, ","))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cert_path": certPath,
		"key_path":  keyPath,
		"hosts":     hosts,
		"not_after": tpl.NotAfter.UTC().Format(time.RFC3339),
		"message":   "自签证书已生成。把 config.yaml 的 tls_cert / tls_key 指向这两个路径，重启面板即可以 HTTPS 提供服务；浏览器会提示不受信任，公网请换正式证书。",
	})
}

// normalizeTLSHosts 去重、去空白，并保证回环地址在列（否则用 127.0.0.1 访问会报证书不匹配）。
func normalizeTLSHosts(in []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for _, h := range in {
		add(h)
	}
	add("127.0.0.1")
	add("::1")
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	return out
}

// handleUploadCert POST /api/tls/upload（管理员）—— 上传正式证书（Nginx 格式 .pem + .key）。
//
// 校验三件事再落盘：能配对（tls.X509KeyPair）、证书未过期、至少含一个 SAN。
// 第三条容易被忽略：现代浏览器**只看 SAN，不看 CN** —— 只有 CN 的证书在 Chrome 里
// 会直接报「证书无效」，而用户往往以为是面板的问题。
func (s *Server) handleUploadCert(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var req struct {
		Cert string `json:"cert"`
		Key  string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if strings.TrimSpace(req.Cert) == "" || strings.TrimSpace(req.Key) == "" {
		writeErr(w, http.StatusBadRequest, "证书与私钥都要提供")
		return
	}
	pair, err := tls.X509KeyPair([]byte(req.Cert), []byte(req.Key))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "证书与私钥不匹配或格式不对："+err.Error())
		return
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "解析证书失败："+err.Error())
		return
	}
	if time.Now().After(leaf.NotAfter) {
		writeErr(w, http.StatusBadRequest, "这张证书已经过期（到期 "+leaf.NotAfter.Format("2006-01-02")+"）")
		return
	}
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		writeErr(w, http.StatusBadRequest,
			"证书里没有 SAN（Subject Alternative Name）。现代浏览器只看 SAN、不看 CN，这种证书会被直接判为无效")
		return
	}
	dir := s.certsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	certPath := filepath.Join(dir, "panel.crt")
	keyPath := filepath.Join(dir, "panel.key")
	if err := os.WriteFile(certPath, []byte(req.Cert), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(keyPath, []byte(req.Key), 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "upload_cert", leaf.Subject.CommonName, leaf.NotAfter.Format("2006-01-02"))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cert_path": certPath,
		"key_path":  keyPath,
		"subject":   leaf.Subject.CommonName,
		"dns":       leaf.DNSNames,
		"ips":       fmtIPs(leaf),
		"not_after": leaf.NotAfter.UTC().Format(time.RFC3339),
		"message":   "证书已保存。把 config.yaml 的 tls_cert / tls_key 指向这两个路径后重启面板。",
	})
}

// fmtIPs 把证书里的 IP SAN 转成字符串列表（响应里给人看）。
func fmtIPs(c *x509.Certificate) []string {
	out := make([]string, 0, len(c.IPAddresses))
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

// handleTLSStatus GET /api/tls/status（管理员）——「当前证书是什么状态」。
//
// 界面需要它回答三个问题：配没配、配的是哪张、还能用多久。
// 尤其是第三点：证书到期不配置告警的话，用户会在某天突然发现面板打不开了。
func (s *Server) handleTLSStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	out := map[string]interface{}{
		"cert_path":  s.tlsCert,
		"key_path":   s.tlsKey,
		"configured": s.tlsCert != "" && s.tlsKey != "",
		"https_now":  r.TLS != nil,
	}
	// 证书文件可能存在但没被 config 引用（比如刚生成、还没改配置）——两种都告诉界面
	dir := s.certsDir()
	genCert := filepath.Join(dir, "panel.crt")
	if _, err := os.Stat(genCert); err == nil {
		out["generated_path"] = genCert
	}
	path := s.tlsCert
	if path == "" {
		path = genCert
	}
	if raw, err := os.ReadFile(path); err == nil {
		if blk, _ := pem.Decode(raw); blk != nil {
			if leaf, err := x509.ParseCertificate(blk.Bytes); err == nil {
				dns, ips := leaf.DNSNames, fmtIPs(leaf)
				out["subject"] = leaf.Subject.CommonName
				out["dns_names"] = dns
				out["ip_names"] = ips
				out["not_after"] = leaf.NotAfter.UTC().Format(time.RFC3339)
				out["days_left"] = int(time.Until(leaf.NotAfter).Hours() / 24)
				out["self_signed"] = len(leaf.Issuer.CommonName) > 0 && leaf.Issuer.CommonName == leaf.Subject.CommonName
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// certsDir 证书落盘目录（<数据目录>/certs），**保证是绝对路径**。
//
// 为什么强调绝对：面板的 DataDir 常写成相对值（安装脚本就是 "data"），
// 于是 avatarDir 也是相对的，拼出来的 "data/certs/panel.crt" 只在
// WorkingDirectory 正确时才有效 —— 而它会直接写进用户要填进 config.yaml 的路径里，
// 相对的等于给了个错答案（真机验证抓到过）。
func (s *Server) certsDir() string {
	dir := filepath.Join(s.avatarDir, "..", "certs")
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}