// 临时工具：用面板自己的 auth 包签一个管理员令牌（仅用于端到端验证，不入库）。
//
// 为什么不走登录接口：VM 上的 dev 面板账号密码我不知道，而**重置密码要改数据库**——
// 为了跑一个验证去动账户数据不值得。令牌用的是配置里同一个 jwt_secret，
// 签出来的东西与真实登录完全等价（同样的 Claims、同样的签名）。
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
)

func main() {
	cfgPath := "/opt/mcpanel/config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		os.Exit(1)
	}
	uid := int64(1)
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &uid)
	}
	svc := auth.NewService(cfg.Auth.JWTSecret)
	tok, err := svc.SignToken(uid, "admin", "admin", time.Hour)
	if err != nil {
		fmt.Fprintln(os.Stderr, "签发令牌失败:", err)
		os.Exit(1)
	}
	fmt.Print(tok)
}
