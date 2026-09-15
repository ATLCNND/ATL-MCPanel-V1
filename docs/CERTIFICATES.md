# 面板 HTTPS 证书配置指南

ATL-MCPanel 支持两种 HTTPS 部署方式，证书既可用**自签**，也可用**阿里云等 CA 签发的正式证书**。

---

## 1. 配置方式（config.yaml）

```yaml
server:
  listen: ":8080"            # HTTP（内网/本地访问，可保留）
  tls_listen: ":8443"        # HTTPS 监听端口
  tls_cert: "/opt/mcpanel/certs/panel.crt"   # 证书
  tls_key:  "/opt/mcpanel/certs/panel.key"   # 私钥
  external_url: "https://面板域名:端口"
```

两种模式：

| 模式 | 配置 | 结果 |
|------|------|------|
| **双端口**（推荐） | 同时配 `listen` 与 `tls_listen` | HTTP:8080 + HTTPS:8443 并存 |
| **单端口** | 只配 `tls_cert`/`tls_key` | `listen` 端口直接跑 HTTPS |

修改后重启面板生效：`systemctl restart atlmcpanel-panel`

> **面板穿透会自动跟随**：配置了 `tls_listen` 后，面板穿透的转发目标会自动从 HTTP 端口切到 HTTPS 端口，
> 因此公网端口对外即为加密访问，无需额外配置。

---

## 2. 自签证书（内网/测试，立即可用）

```bash
# 在节点上执行（会带本机 IP 与 localhost 的 SAN）
sudo bash scripts/gen-cert.sh /opt/mcpanel/certs
```

或用其它域名/IP：

```bash
sudo bash scripts/gen-cert.sh /opt/mcpanel/certs panel.example.com 10.0.0.5
```

**注意**：自签证书浏览器会提示「不受信任」，需手动信任。
现代浏览器不再接受仅 CN 的证书，必须包含 SAN（脚本已自动处理）。

---

## 3. 阿里云免费证书（推荐用于生产）

阿里云「数字证书管理服务（SSL 证书）」提供**免费 DV 证书**，完全可以用于本面板。

### 3.1 申请

1. 登录阿里云 → **数字证书管理服务** → **SSL 证书** → **免费证书**
2. 点击「创建证书」，填写要绑定的**域名**（如 `panel.example.com`）
3. 完成域名验证（DNS 解析验证或文件验证）
4. 等待签发（免费 DV 通常几分钟内）

> 免费证书有效期通常为 3 个月，到期需重新申请；建议设置到期提醒。

### 3.2 下载并部署

1. 在证书列表点击 **下载**，服务器类型选择 **Nginx**
2. 得到压缩包，内含：
   - `xxx.pem`（证书链）
   - `xxx.key`（私钥）
3. 上传到面板所在机器，例如 `/opt/mcpanel/certs/`：

```bash
# 在本机执行（把文件传到面板服务器）
scp panel.example.com.pem root@<面板IP>:/opt/mcpanel/certs/panel.crt
scp panel.example.com.key root@<面板IP>:/opt/mcpanel/certs/panel.key
```

> 文件名不必改，只要 `config.yaml` 里的 `tls_cert` / `tls_key` 指向实际文件即可。
> `.pem` 与 `.crt` 内容格式一致，可直接使用。

4. 确认权限并重启面板：

```bash
chmod 600 /opt/mcpanel/certs/panel.key
chmod 644 /opt/mcpanel/certs/panel.crt
systemctl restart atlmcpanel-panel
```

5. 验证（应无证书警告）：

```bash
curl -I https://panel.example.com:8443/
# 或用浏览器打开，地址栏应显示锁形图标
```

### 3.3 域名解析

证书绑定的是域名，因此**必须通过域名访问**：

- **方式 A（TCP 穿透，最简）**：把域名解析到公网服务器 IP，
  浏览器访问 `https://panel.example.com:公网端口`
  （证书校验域名，端口任意）
- **方式 B（HTTPS vhost，URL 无端口）**：frps 需配置 `vhostHTTPSPort = 443`，
  面板穿透使用 `https` 模式并填写自定义域名，
  浏览器直接访问 `https://panel.example.com`

> 方式 A 无需修改 frps 配置，接入最快；方式 B 的 URL 更干净但要求 443 端口可用。

---

## 4. 常见问题

**Q：能否直接用阿里云证书 + TCP 穿透？**
可以。证书校验的是**域名**而非端口，所以 `https://你的域名:18080` 也能正常显示可信。

**Q：面板内网 HTTP (8080) 还需要保留吗？**
建议保留：内网访问、健康检查、脚本调用更方便，且不经过公网。
公网只暴露 HTTPS 端口即可。

**Q：frpc 侧的 TLS 终止（https 模式 + https2http 插件）与这里有何区别？**
- **本文方式**：TLS 在面板进程内终止，配置简单，证书在面板侧管理（推荐）
- **frpc 插件方式**：TLS 在 frpc 侧终止，面板可以只跑 HTTP；
  适合面板不便于配置证书的场景，但要求 frps 开启 `vhostHTTPSPort` 并使用域名

**Q：免费证书快到期时怎么办？**
重新申请并替换 `/opt/mcpanel/certs/` 下的两个文件，`systemctl restart atlmcpanel-panel` 即可。
