// Package auth 提供用户注册、登录、JWT 签发与校验。
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Claims JWT 载荷。
type Claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	// TokenVersion 与 users.token_version 对应：库里对不上就说明令牌已作废
	// （改过密码 / 账号被处理过）。见 internal/panel/db/migrate.go 版本 23。
	TokenVersion int64 `json:"tv"`
	jwt.RegisteredClaims
}

// Service 认证服务。
type Service struct {
	secret []byte
}

// NewService 创建认证服务。
func NewService(secret string) *Service {
	return &Service{secret: []byte(secret)}
}

// HashPassword 用 bcrypt 加密密码。
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword 校验密码。
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// SignToken 签发 JWT。
//
// tokenVersion 来自 users.token_version：它是令牌的"世代号"，改密码等操作会
// 把它推进一位，于是此前签发的令牌全部失效（撤销机制，见 migrate.go v23）。
func (s *Service) SignToken(userID int64, username, role string, tokenVersion int64, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID:       userID,
		Username:     username,
		Role:         role,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "atlmcpanel",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

// ParseToken 解析并校验 JWT。
func (s *Service) ParseToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}
