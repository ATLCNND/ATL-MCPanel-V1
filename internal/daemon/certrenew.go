package daemon

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 节点证书的**自动续期**。
//
// ---------------------------------------------------------------------------
// 为什么必须有这一段（2026-11-09 盘点 1.0.0-preview 未做项时发现）
// ---------------------------------------------------------------------------
// 面板签发的节点证书只有 **30 天**（`internal/panel/httpapi/enroll.go` 的
// enrollCertTTL），响应里也老老实实带回 `renew_after`（到期前 7 天），
// 但全仓库**只有命令行把它打印出来**（`cmd/daemon/main.go`："建议在该时间前续期"），
// 运行期没有任何代码去用它。于是：
//
//	证书过期 → mTLS 握手失败 → 节点与面板彻底失联，
//	而面板上只会显示"节点离线"，节点日志里则是 TLS 错误。
//
// 生产机（202.189.15.98）的 node-001 证书到期时间就是签发日 + 30 天：
// 一个部署完就没人碰的节点，会在一个月后的某个凌晨自己掉线。
//
// 续期用的是 `/api/enroll/renew`，凭据是**私钥本身**（不需要 token、也不需要
// 当前证书仍有效 —— 面板只校验「node_uid 未撤销」+「CSR 公钥指纹与首次登记一致」，
// 而 CSR 自带自签名校验，签得出它就证明持有私钥）。所以即使节点停机很久、
// 证书已经过期，只要 `node.key` 还在，重连时也能自己续回来。

const (
	// certRenewBefore 剩余有效期少于这个值就续期。
	// 取 7 天与面板的 `renew_after`（TTL-7天）一致：留出足够余量应对
	// "面板当时不可达 / 节点关机"，又不至于每天续一次。
	certRenewBefore = 7 * 24 * time.Hour
	// certRenewCheckInterval 巡检间隔。证书是 30 天量级的东西，
	// 6 小时一次足够及时，也不会给面板添任何负担。
	certRenewCheckInterval = 6 * time.Hour
)

// RenewNodeCert 用本机已登记的客户端私钥换一张新证书（同一把钥匙，只换有效期）。
//
// 与 PickupCert 的区别只在于用哪把钥匙：
//   - PickupCert 用 `join.key`（③ 节点发起时留下的接入密钥，此时还没有 node.key）；
//   - 这里用 `node.key`（已经在跑的那张证书对应的私钥，面板登记的公钥指纹就是它）。
//
// 成功后把新证书与 CA 原子写回 CertDir；**私钥不动**（面板要求指纹一致）。
func RenewNodeCert(panelBase, stateDir, certDir, keyPath, nodeName string) (certPath, caPath string, expires time.Time, err error) {
	if strings.TrimSpace(panelBase) == "" {
		return "", "", time.Time{}, fmt.Errorf("未配置面板地址（daemon.panel_url），无法自动续期")
	}
	uid, err := readNodeUID(stateDir)
	if err != nil {
		return "", "", time.Time{}, err
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("读取节点私钥失败: %w", err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return "", "", time.Time{}, fmt.Errorf("节点私钥 %s 不是合法的 PEM", keyPath)
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("解析节点私钥失败: %w", err)
	}
	csrPEM, err := csrFromKey(key, nodeName)
	if err != nil {
		return "", "", time.Time{}, err
	}

	body, _ := json.Marshal(map[string]string{"node_uid": uid, "csr": string(csrPEM)})
	url := strings.TrimRight(panelBase, "/") + "/api/enroll/renew"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", time.Time{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("请求面板失败: %w", err)
	}
	defer httpResp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	var resp enrollResponse
	_ = json.Unmarshal(rawResp, &resp)
	if httpResp.StatusCode != http.StatusOK {
		msg := resp.Error
		if msg == "" {
			msg = truncate(string(rawResp), 200)
		}
		err := fmt.Errorf("面板拒绝了这次续期（HTTP %d）：%s", httpResp.StatusCode, msg)
		// 403 在真机上只有两种来路：节点被撤销，或者本机证书不是面板登记的那把
		// 钥匙（2026-11-09 开发机上真踩到：磁盘上是早期装的长效证书，而
		// node_enrollments 里记的是另一次接入的公钥指纹）。两种都得人工介入，
		// 光把面板原话抛上去，运维看不出下一步该做什么。
		if httpResp.StatusCode == http.StatusForbidden {
			return "", "", time.Time{}, fmt.Errorf("%w；这需要人工介入：若提示「已被撤销」请让管理员在面板上重新接入；"+
				"若提示「公钥指纹不一致」，说明本机 certs/node.key 与面板登记的不是同一把钥匙，"+
				"请用 atl-daemon enroll --panel <面板地址> --token <一次性令牌> 重新走一次凭证交换", err)
		}
		return "", "", time.Time{}, err
	}
	if resp.Cert == "" || resp.CA == "" {
		return "", "", time.Time{}, fmt.Errorf("面板返回的证书或 CA 为空")
	}

	if certDir == "" {
		certDir = "certs"
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return "", "", time.Time{}, err
	}
	certPath = filepath.Join(certDir, "node.crt")
	caPath = filepath.Join(certDir, "ca.crt")
	// 原子写：巡检与 TLS 握手是并发的，不能让它读到写了一半的证书。
	if err := writeFileAtomic(certPath, []byte(resp.Cert), 0o644); err != nil {
		return "", "", time.Time{}, err
	}
	if err := writeFileAtomic(caPath, []byte(resp.CA), 0o644); err != nil {
		return "", "", time.Time{}, err
	}

	if leaf := firstCertFromPEM([]byte(resp.Cert)); leaf != nil {
		expires = leaf.NotAfter
	} else if resp.ExpiresAt != "" {
		if t, e := time.Parse(time.RFC3339, resp.ExpiresAt); e == nil {
			expires = t
		}
	}
	return certPath, caPath, expires, nil
}

// panelURLFile 接入时记下面板 HTTP 地址的文件名（放在 state 目录，与 node_uid 同级）。
const panelURLFile = "panel_url"

// savePanelURL 把接入时用的面板地址记下来，供自动续期兜底使用。
//
// 为什么需要这个：`daemon.panel_url` 是 0.11.3 才加的配置键，而**老安装升级上来**
// 的节点配置里没有它；面板发起的 ② 接入又只在命令行 `--panel` 里出现过一次地址。
// 不记下来的话，这些节点的自动续期会一直因为"未配置面板地址"而跳过。
func savePanelURL(stateDir, panelBase string) error {
	panelBase = strings.TrimSpace(panelBase)
	if stateDir == "" {
		stateDir = "state"
	}
	if panelBase == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, panelURLFile), []byte(panelBase+"\n"), 0o600)
}

// readPanelURL 读接入时记下的面板地址（没有就返回空串）。
func readPanelURL(stateDir string) string {
	if stateDir == "" {
		stateDir = "state"
	}
	b, err := os.ReadFile(filepath.Join(stateDir, panelURLFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeFileAtomic 写临时文件再 rename，避免读者看到半截内容。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// firstCertFromPEM 取 PEM 里的第一张证书。
func firstCertFromPEM(pemBytes []byte) *x509.Certificate {
	for {
		var blk *pem.Block
		blk, pemBytes = pem.Decode(pemBytes)
		if blk == nil {
			return nil
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil
		}
		return cert
	}
}

// readCertLeaf 读证书文件里的第一张证书（用来看到期时间）。
func readCertLeaf(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	leaf := firstCertFromPEM(raw)
	if leaf == nil {
		return nil, fmt.Errorf("%s 里没有可解析的证书", path)
	}
	return leaf, nil
}

// maybeRenewNodeCert 到期前把证书换掉。返回是否真的续了。
//
// 三种情况直接返回 false 不折腾：
//   - 没开 mTLS（明文部署本来就没有证书）；
//   - 证书还早（剩余 > certRenewBefore）；
//   - 没配 panel_url —— 但这种情况**要报出来**：它意味着"这个节点永远不会自动续期"，
//     与其一个月后神秘掉线，不如现在就在日志里说明白。
func (d *Daemon) maybeRenewNodeCert() (bool, error) {
	if !d.cfg.TLS || d.cfg.CertFile == "" {
		return false, nil
	}
	leaf, err := readCertLeaf(d.cfg.CertFile)
	if err != nil {
		// 读不到证书由启动路径（ensureNodeCerts / transportCredentials）报错，
		// 巡检这里只记一条，不参与判定。
		return false, fmt.Errorf("读取节点证书失败: %w", err)
	}
	remaining := time.Until(leaf.NotAfter)
	if remaining > certRenewBefore {
		return false, nil
	}
	// 面板地址有两个来源：配置里的 daemon.panel_url 优先，其次是接入时记下的
	// state/panel_url（老安装升级上来时配置里没有这个键，见 savePanelURL）。
	panelBase := strings.TrimSpace(d.cfg.PanelURL)
	if panelBase == "" {
		panelBase = readPanelURL(d.cfg.StateDir)
	}
	if panelBase == "" {
		return false, fmt.Errorf("节点证书将于 %s 到期（剩余 %s），但没有面板 HTTP 地址可用来续期："+
			"请在 config.yaml 里配置 daemon.panel_url（例如 http://10.0.0.5:25080，启用安全入口时带上 /<入口>）；"+
			"到期后节点将与面板失联", leaf.NotAfter.Format("2006-01-02 15:04"), remaining.Round(time.Hour))
	}

	certDir := filepath.Dir(d.cfg.CertFile)
	keyPath := d.cfg.KeyFile
	if keyPath == "" {
		keyPath = filepath.Join(certDir, "node.key")
	}
	certPath, caPath, expires, err := RenewNodeCert(panelBase, d.cfg.StateDir, certDir, keyPath, d.cfg.NodeID)
	if err != nil {
		return false, err
	}
	d.log.Info("节点证书已续期",
		"cert", certPath, "ca", caPath,
		"old_expires", leaf.NotAfter.Format(time.RFC3339),
		"new_expires", expires.Format(time.RFC3339),
		"remaining", time.Until(expires).Round(time.Hour).String())
	return true, nil
}

// renewCertLoop 周期性巡检证书。启动时先看一次（关机很久的节点一上线就该续）。
func (d *Daemon) renewCertLoop() {
	tryOnce := func(tag string) {
		renewed, err := d.maybeRenewNodeCert()
		if err != nil {
			d.log.Warn("节点证书续期未成功（不影响当前运行，下一轮再试）", "stage", tag, "err", err.Error())
			return
		}
		if renewed {
			d.log.Info("续期后的证书已落盘（下次 TLS 握手自动使用新证书，无需重启）", "stage", tag)
		}
	}
	tryOnce("启动")
	ticker := time.NewTicker(certRenewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.shutdown:
			return
		case <-ticker.C:
			tryOnce("巡检")
		}
	}
}

// ---------------------------------------------------------------------------
// 证书热加载
// ---------------------------------------------------------------------------
// 续期把新证书写到同一个路径，但**已经建好的连接不会自动换证书**：下一次握手才会。
// 而 mTLS 配置在进程启动时就把证书读进内存了 —— 不热加载的话，续期等于白续
// （文件是新的，进程还用旧的，重启才能生效；而重启 Daemon 会杀掉全部实例）。
//
// 所以客户端侧挂 GetClientCertificate、服务端侧挂 GetCertificate，
// 每次都 stat 一下文件，变了才重新 LoadX509KeyPair。

// fileStamp 用来判断文件是否被换过。
type fileStamp struct {
	size int64
	mod  time.Time
}

func statStamp(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime()}
}

// reloadableKeyPair 按文件 mtime 缓存证书；文件换了就重新加载。
type reloadableKeyPair struct {
	certFile, keyFile string
	log               *slog.Logger

	mu        sync.Mutex
	cert      *tls.Certificate
	certStamp fileStamp
	keyStamp  fileStamp
}

func newReloadableKeyPair(certFile, keyFile string, log *slog.Logger) *reloadableKeyPair {
	return &reloadableKeyPair{certFile: certFile, keyFile: keyFile, log: log}
}

// get 返回当前证书；文件变了会重新加载。
//
// 加载失败时**继续用旧证书**：续期写坏文件（磁盘满、权限错）不该让
// 一个本来能跑的节点突然握手失败。旧证书到期前都还能用。
func (r *reloadableKeyPair) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, ks := statStamp(r.certFile), statStamp(r.keyFile)
	if r.cert != nil && cs == r.certStamp && ks == r.keyStamp {
		return r.cert, nil
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			if r.log != nil {
				r.log.Warn("重新加载节点证书失败，继续使用内存里的旧证书",
					"cert", r.certFile, "err", err.Error())
			}
			return r.cert, nil
		}
		return nil, err
	}
	if r.cert != nil && r.log != nil {
		r.log.Info("已重新加载节点证书", "cert", r.certFile)
	}
	r.cert = &pair
	r.certStamp, r.keyStamp = cs, ks
	return r.cert, nil
}
