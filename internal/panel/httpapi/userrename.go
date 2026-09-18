package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxUsernameRunes 用户名长度上限（按**字符**数，不是字节数 —— 中文名不该因为
// UTF-8 一个字符占 3 字节就被判超长）。
const maxUsernameRunes = 32

// handleSetUsername 改用户名。
// PUT /api/users/{id}/username  body: {"username": "新名字"}
//
// 谁能改：本人改自己，或总管理员改任何人。
//
// 为什么以前不能改、现在能改：
//
//	用户名在此之前是**唯一标识**（`users.username UNIQUE`），凡是"记住这个人是谁"
//	的地方都拿它当钥匙 —— 界面上按用户名匹配授权、按用户名比较"是不是我自己"。
//	一旦能改名，这些地方立刻全错位，所以先补齐了 **UID（users.id）**：
//	  · 登录响应带上 `id`，前端把它存进登录态；
//	  · 后端授权**本来就**只认 `uid`（middleware 只把 UserID/Role 放进 context），
//	    授权表（instance_assignments / node_users / node_user_ports）也全部按 user_id 存。
//	于是用户名退化成"可改的显示名"，改名不再破坏任何关联 —— 这正是加 UID 的目的。
//
// 改完的即时效果：
//   - `/api/auth/me` 从数据库读用户名，所以刷新页面就是新名字；
//   - **手里的令牌仍然带旧用户名**（JWT 里 username 只是个备注字段，授权不用它）。
//     不影响权限，只影响"令牌里的显示名"，重新登录后彻底一致。响应里明说这点。
//
// 不做的两件事，以及原因：
//  1. **不做大小写不敏感的查重**：`username` 是 `TEXT UNIQUE`，SQLite 默认二进制
//     比较，注册接口也只做精确查重 —— 这里跟着精确比较，行为才可预测。
//     代价是 "Bob" 和 "bob" 可以并存（登录限流按小写归并，两者共用一个桶），
//     这是注册接口就有的老问题，不在这里顺手改掉。
//  2. **不改动注册接口的校验**：老账号里可能已存在带空格等不规范的名字，
//     收紧注册校验不会影响它们（只影响新建），但那是另一个改动，避免混在一起。
func (s *Server) handleSetUsername(w http.ResponseWriter, r *http.Request) {
	targetID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "user id 无效")
		return
	}
	// 本人可改自己；改别人必须是总管理员。
	if targetID != currentUserID(r) && roleOf(r) != RoleAdmin {
		writeErr(w, http.StatusForbidden, "只能修改自己的用户名")
		return
	}

	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	newName := strings.TrimSpace(req.Username)
	if msg := checkUsername(newName); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	var oldName string
	if err := s.db.QueryRow(`SELECT username FROM users WHERE id = ?`, targetID).Scan(&oldName); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if oldName == newName {
		// 不算错误：前端可能只是把原值回传。直接说"没变"，别报冲突。
		writeJSON(w, http.StatusOK, map[string]string{
			"message":  "用户名未变化",
			"username": newName,
		})
		return
	}

	// 精确查重：给出"已被占用"这种能看懂的话，而不是等 UNIQUE 约束抛 SQL 错误。
	var taken int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, newName).Scan(&taken)
	if taken > 0 {
		writeErr(w, http.StatusConflict, "用户名已被占用："+newName)
		return
	}

	if _, err := s.db.Exec(`UPDATE users SET username = ? WHERE id = ?`, newName, targetID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 审计里同时留下旧名与新名：只留新名的话，翻旧日志就对不上"当时那个人是谁"。
	s.audit(r, "rename_user", oldName+" → "+newName, "user_id="+strconv.FormatInt(targetID, 10))

	msg := "用户名已改为 " + newName + "（UID " + strconv.FormatInt(targetID, 10) + " 不变，实例授权与端口配额不受影响）"
	if targetID == currentUserID(r) {
		msg += "；当前登录状态仍然有效，刷新页面即可看到新名字"
	} else {
		msg += "；对方刷新页面即可看到新名字，无需重新登录"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "username": newName})
}

// checkUsername 校验用户名，返回空字符串表示通过，否则返回给用户看的原因。
//
// 只拦两类字符：空白字符与控制字符。理由是它们会让名字在界面、日志、CSV 导出里
// 变得歧义或直接错行（"a b" 与 "a  b" 看起来一样、"a\nb" 能把日志切两行）；
// 中文、emoji、符号一律放行 —— 没有正当理由替用户决定名字长什么样。
func checkUsername(name string) string {
	if name == "" {
		return "用户名不能为空"
	}
	if !utf8.ValidString(name) {
		return "用户名包含非法字符"
	}
	if utf8.RuneCountInString(name) > maxUsernameRunes {
		return "用户名最长 " + strconv.Itoa(maxUsernameRunes) + " 个字符"
	}
	for _, c := range name {
		if unicode.IsSpace(c) {
			return "用户名不能包含空格或换行"
		}
		if unicode.IsControl(c) {
			return "用户名不能包含控制字符"
		}
	}
	return ""
}
