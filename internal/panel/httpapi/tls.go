package httpapi

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 面板自签证书（M5 / F7）。
//
// 为什么要有它：给面板配 HTTPS 的前提是「有一张证书」。多数人手上没有正式证书，
// 而自签证书能立刻解决一个具体问题 —— 控制台 WebSocket 在 HTTPS 下可用
// （自签会让浏览器警告，点过「继续访问」之后 WS 是能用的）。
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
	pair := req.Cert
	keyPEM := req.Key
	leaf, err := checkCertPair(pair, keyPEM)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := s.certsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	certPath, keyPath, err := s.storePanelCert(dir, req.Cert, req.Key)
	if err != nil {
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

// storePanelCert 把已经校验过的证书与私钥落盘（certs/panel.crt 0644 + panel.key 0600）。
func (s *Server) storePanelCert(dir, certPEM, keyPEM string) (certPath, keyPath string, err error) {
	certPath = filepath.Join(dir, "panel.crt")
	keyPath = filepath.Join(dir, "panel.key")
	if err := os.WriteFile(certPath, []byte(certPEM), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, []byte(keyPEM), 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

// checkCertPair 校验一对 PEM：能配对、未过期、含 SAN。
//
// 抽出来是因为「粘贴 PEM」与「上传压缩包」两条路都必须过同样的三道关 ——
// 只在其中一条上校验，等于给另一条留了个后门（尤其 SAN 那条，
// 只有 CN 的证书在 Chrome 里直接判无效，用户会以为面板坏了）。
func checkCertPair(certPEM, keyPEM string) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, errorf("证书与私钥不匹配或格式不对：%v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errorf("解析证书失败：%v", err)
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, errorf("这张证书已经过期（到期 %s）", leaf.NotAfter.Format("2006-01-02"))
	}
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		return nil, errorf("证书里没有 SAN（Subject Alternative Name）。现代浏览器只看 SAN、不看 CN，这种证书会被直接判为无效")
	}
	return leaf, nil
}

// errorf 只是 fmt.Errorf 的别名（本文件其它地方也用 fmt 风格拼消息）。
func errorf(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
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

// handleUploadCertArchive POST /api/tls/upload-archive（管理员，multipart，字段名 file）
//
// 从**压缩包**（或单个合并 PEM）里把证书和私钥找出来。
//
// 为什么按内容判断、不看文件名：从 IDC 后台下载的证书包命名五花八门
// （cert.crt / fullchain.pem / server.crt / privkey.key / *-privatekey.pem /
// 甚至 cert.crt + key.txt 这种），靠后缀猜一定会猜错，猜错时的报错
// （"证书与私钥不匹配"）又会让人以为自己拿错了文件。
// 所以这里只认两条：文本里有没有 CERTIFICATE 块、有没有 PRIVATE KEY 块。
//
// 大小限制：整包 2MB、单个条目 256KB。证书文件是纯文本的几 KB，
// 超过这个数量级的多半是上传错了东西（比如把整个网站备份丢上来）。
func (s *Server) handleUploadCertArchive(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "上传解析失败："+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请选择要上传的证书压缩包（zip）或合并后的 PEM 文件")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 2<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取上传内容失败："+err.Error())
		return
	}
	certPEM, keyPEM, source, err := extractCertKey(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	leaf, err := checkCertPair(certPEM, keyPEM)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := s.certsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	certPath, keyPath, err := s.storePanelCert(dir, certPEM, keyPEM)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "upload_cert", leaf.Subject.CommonName,
		hdr.Filename+" → "+source+"，到期 "+leaf.NotAfter.Format("2006-01-02"))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cert_path": certPath,
		"key_path":  keyPath,
		"subject":   leaf.Subject.CommonName,
		"dns":       leaf.DNSNames,
		"ips":       fmtIPs(leaf),
		"not_after": leaf.NotAfter.UTC().Format(time.RFC3339),
		"used":      source,
		"message":   "已从「" + hdr.Filename + "」里取到证书（" + source + "）并保存。把 config.yaml 的 tls_cert / tls_key 指向这两个路径后重启面板。",
	})
}

// splitPEMBlocks 把一个"证书 + 私钥"混在一起的 PEM 拆成两段。
//
// 用 pem.Decode 逐块走，而不是按行找 BEGIN/END：证书链里有多张证书，
// 私钥块也可能夹在中间，按行切很容易把块切坏（切坏了报的是
// "格式不对"，用户完全不知道该怎么办）。
func splitPEMBlocks(text string) (certText, keyText string) {
	rest := []byte(text)
	var certs, keys []byte
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		enc := pem.EncodeToMemory(blk)
		if blk.Type == "CERTIFICATE" {
			certs = append(certs, enc...)
		} else if strings.Contains(blk.Type, "PRIVATE KEY") {
			keys = append(keys, enc...)
		}
	}
	return string(certs), string(keys)
}

// extractCertKey 从 zip 或单个 PEM 里取出证书与私钥（按内容识别）。
//
// 返回的 source 是给人看的一句话（用了包里的哪两个文件），
// 直接显示在界面上 —— 上传错文件时这是唯一能让人看出问题的线索。
func extractCertKey(data []byte) (certPEM, keyPEM, source string, err error) {
	type part struct {
		name  string
		text  string
		certs int
		key   bool
	}
	var parts []part
	add := func(name string, b []byte) {
		text := string(b)
		n := strings.Count(text, "-----BEGIN CERTIFICATE-----")
		isKey := strings.Contains(text, "PRIVATE KEY-----")
		if n == 0 && !isKey {
			return // 既不是证书也不是私钥（说明文件、README 之类）直接忽略
		}
		if n > 0 && isKey {
			// 同一个文件里既有证书又有私钥（服务商常给的 combined.pem）：
			// 拆成两份候选，否则下面"私钥文件里不该带证书"的判定会把它两边都排除掉，
			// 用户会看到"没有找到私钥"这种莫名其妙的提示。
			certText, keyText := splitPEMBlocks(text)
			if certText != "" {
				parts = append(parts, part{name: name, text: certText, certs: n})
			}
			if keyText != "" {
				parts = append(parts, part{name: name, text: keyText, key: true})
			}
			return
		}
		parts = append(parts, part{name: name, text: text, certs: n, key: isKey})
	}

	// 先当 zip 试（PK\x03\x04 魔数直接看，比 try-error 干净）
	if len(data) > 4 && data[0] == 'P' && data[1] == 'K' {
		zr, zerr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if zerr != nil {
			return "", "", "", fmt.Errorf("解析 zip 失败：%v", zerr)
		}
		entries := 0
		for _, e := range zr.File {
			if e.FileInfo().IsDir() || e.UncompressedSize64 > 256<<10 {
				continue
			}
			if entries >= 64 {
				break
			}
			rc, oerr := e.Open()
			if oerr != nil {
				continue
			}
			b, rerr := io.ReadAll(io.LimitReader(rc, 256<<10))
			rc.Close()
			if rerr != nil {
				continue
			}
			entries++
			add(e.Name, b)
		}
		if len(parts) == 0 {
			return "", "", "", fmt.Errorf("压缩包里没有找到证书或私钥（要的是 .pem/.crt/.key 这类文本文件）")
		}
	} else {
		// 不是 zip：可能是合并了证书与私钥的单个 PEM
		add("上传的文件", data)
		if len(parts) == 0 {
			return "", "", "", fmt.Errorf("这个文件里既没有 -----BEGIN CERTIFICATE----- 也没有 -----BEGIN PRIVATE KEY-----：请上传包含证书与私钥的 zip 压缩包")
		}
	}

	// 证书候选：含 CERTIFICATE 块的非私钥文件（真世界里私钥文件里也可能带证书，排除掉）
	// 证书块多的优先（fullchain 比单张 leaf 好：浏览器需要中间证书）
	var certCands, keyCands []part
	for _, p := range parts {
		if p.certs > 0 {
			certCands = append(certCands, p)
		}
		if p.key && p.certs == 0 {
			keyCands = append(keyCands, p)
		}
	}
	if len(certCands) == 0 {
		return "", "", "", fmt.Errorf("没有找到证书（缺少 -----BEGIN CERTIFICATE-----）")
	}
	if len(keyCands) == 0 {
		return "", "", "", fmt.Errorf("没有找到私钥（缺少 -----BEGIN ... PRIVATE KEY-----）；若私钥与证书合并在同一个文件里，请把那个文件单独上传")
	}
	sort.SliceStable(certCands, func(i, j int) bool { return certCands[i].certs > certCands[j].certs })

	// 挨个组合试配对：包里常有多套证书（旧证书 + 新证书），
	// 盲目取第一个会配不上，而"配不上"的用户提示毫无意义。
	var lastErr error
	for _, c := range certCands {
		for _, k := range keyCands {
			if _, err := tls.X509KeyPair([]byte(c.text), []byte(k.text)); err != nil {
				lastErr = err
				continue
			}
			return c.text, k.text, c.name + " + " + k.name, nil
		}
	}
	if lastErr != nil {
		return "", "", "", fmt.Errorf("包里的证书与私钥配不上（试过 %d 张证书 × %d 个私钥）：%v",
			len(certCands), len(keyCands), lastErr)
	}
	return "", "", "", fmt.Errorf("包里的证书与私钥配不上")
}
