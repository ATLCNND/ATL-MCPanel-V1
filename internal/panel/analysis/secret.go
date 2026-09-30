// Package analysis 日志分析的"提供方"抽象：把日志交给谁分析、用什么凭据、限速多少。
//
// 它把原来写死的 LogShare 集成拆成几层，每一层都可以单测：
//
//	secret.go   密钥的加密存储（AES-GCM，明文不落库）
//	ssrf.go     出站地址白名单（base_url 是用户可控 URL，面板会带着 key 去请求它）
//	ratelimit.go 按用户的调用配额（保护用户的 key 与我们的公益额度）
//	openai.go   OpenAI 兼容客户端（绝大多数平台都是这个形态）
//	mclogs.go   mclo.gs 保底：不是 AI，只是"把日志变成可分享链接"
//
// 这个包不认识数据库、不认识实例：存储与鉴权都在 httpapi 层。
package analysis

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretBox 负责 API Key 的加密存储。
//
// 为什么必须加密：面板的数据库会被备份、会被拷去排查、会随快照一起走。
// 里面的 API Key 是**能花钱、能读用户数据**的凭据，与节点 SSH 口令同级甚至更高。
// 明文存进去，等于"谁拿到库谁就拿到所有人的 key"。
//
// 为什么用独立密钥文件而不是用户口令派生：面板是多用户系统，
// 用户口令不该能解开平台级密钥（否则一个用户改口令就影响别人的凭据）。
type SecretBox struct {
	key []byte // 32 字节，AES-256
}

// secretKeyLen AES-256 的密钥长度。
const secretKeyLen = 32

// OpenSecretBox 从密钥文件加载主密钥；文件不存在则生成一个（0600）。
//
// 生成而不是报错：面板要能开箱即用；但**生成后必须持久化**，否则重启后
// 已存的 key 全部解不开（现象是"保存过的 API Key 突然失效"）。
func OpenSecretBox(path string) (*SecretBox, error) {
	if path == "" {
		return nil, errors.New("密钥文件路径为空")
	}
	if b, err := os.ReadFile(path); err == nil {
		key, err := decodeKey(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, fmt.Errorf("读取分析密钥失败: %w", err)
		}
		return &SecretBox{key: key}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取分析密钥失败: %w", err)
	}

	key := make([]byte, secretKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成分析密钥失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建密钥目录失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("写入分析密钥失败: %w", err)
	}
	return &SecretBox{key: key}, nil
}

func decodeKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("密钥不是合法的十六进制: %w", err)
	}
	if len(b) != secretKeyLen {
		return nil, fmt.Errorf("密钥长度应为 %d 字节，实际 %d", secretKeyLen, len(b))
	}
	return b, nil
}

// Encrypt 加密一段明文（AES-GCM，随机 nonce 前置）。
//
// 空串返回 nil：调用方用"没有密文"表示"这条提供方没有 key"（例如 LogShare/mclo.gs 不需要）。
func (b *SecretBox) Encrypt(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	gcm, err := b.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce 失败: %w", err)
	}
	// nonce 明文前置：解密时先取它，再解后面那段
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Decrypt 解密（Encrypt 的逆操作）。空输入返回空串，不算错误。
func (b *SecretBox) Decrypt(enc []byte) (string, error) {
	if len(enc) == 0 {
		return "", nil
	}
	gcm, err := b.gcm()
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(enc) < ns {
		return "", errors.New("密文长度不足（可能不是本面板写入的）")
	}
	nonce, ct := enc[:ns], enc[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// 这里最常见的原因是**密钥文件被换过**（例如把 data/ 拷到别的机器）。
		// 说清楚这一点，比抛一句 "cipher: message authentication failed" 有用得多。
		return "", errors.New("解密失败：密钥文件与分析密钥不匹配（data/analysis.key 是否被替换或丢失？）")
	}
	return string(plain), nil
}

func (b *SecretBox) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KeyHint 取 API Key 的末 4 位，供界面显示（永不回显完整 key）。
//
// 短于 8 位时只回 "****"：那种长度多半是填错了，回显末 4 位等于泄露了大半。
func KeyHint(key string) string {
	k := strings.TrimSpace(key)
	if len(k) < 8 {
		return "****"
	}
	return "****" + k[len(k)-4:]
}
