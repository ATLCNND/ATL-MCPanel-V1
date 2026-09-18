package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxInstanceNameRunes 实例显示名长度上限（按字符数，不按字节数）。
//
// 与用户名取同一个上限，理由也一样：这是给人看的短标识，
// 太长会在列表、页签标题里折行。
const maxInstanceNameRunes = 32

// handleRenameInstance 改实例的**显示名**（面板数据库 instances.name）。
// PUT /api/instances/{id}  body: {"name": "新名字"}
//
// 为什么改的是 name 而不是 id：
//
//	instance_id 是目录名（<节点实例目录>/<instance_id>），也是所有关联的钥匙 ——
//	授权（instance_assignments）、公网端口（tunnels）、启动记录、许可证。
//	把它改掉等于给实例"搬家再换户籍"，磁盘上的目录、已经在跑的进程、
//	已下发的隧道定义全都要跟着动，失败一次就可能留下半死不活的实例。
//	而用户真正想改的只是**列表和详情页上显示的那行字**，所以只改显示名。
//
// 与用户名的区别：用户名是登录名、要唯一；实例显示名**允许重名**
//
//	（同一个人的两台"生存服"没理由被禁止），也不会影响任何关联 —— 它就是个标签。
//
// Daemon 侧的 instance.json 里也有个 name 字段，那是建实例时写下的快照，
// 之后不再更新（面板数据库才是权威）。为避免"两个名字对不上"的困惑，
// 该文件已从文件管理中隐藏（见 daemon/grpcapi/file.go 的 protectedFiles）。
func (s *Server) handleRenameInstance(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageInstanceSettings(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "无权修改该实例")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	newName := strings.TrimSpace(req.Name)
	if msg := checkInstanceName(newName); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	var oldName string
	if err := s.db.QueryRow(`SELECT name FROM instances WHERE instance_id = ?`, instanceID).
		Scan(&oldName); err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	if oldName == newName {
		writeJSON(w, http.StatusOK, map[string]string{"message": "名称未变化", "name": newName})
		return
	}

	if _, err := s.db.Exec(`UPDATE instances SET name = ? WHERE instance_id = ?`, newName, instanceID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 审计里同时留下旧名与新名：只留新名的话，翻旧日志就对不上"当时那台是哪台"。
	s.audit(r, "rename_instance", instanceID, oldName+" → "+newName)

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "已改名为 " + newName + "（实例 ID 与公网端口不变）",
		"name":    newName,
	})
}

// checkInstanceName 校验实例显示名，返回空字符串表示通过。
//
// 与用户名校验的区别是**允许空格**：服务器名字写成
// "我的生存服 1.20.1" 是很自然的表达，拦掉空格只会逼用户用下划线。
// 一并允许中文、emoji 与符号；只拦控制字符（会把日志/CSV/JSON 里的行切坏）
// 与格式类空白（制表符、换行、不换行空格等肉眼看不出来、却会让名字
// 在界面上对不齐或"看起来一模一样"的字符）。
func checkInstanceName(name string) string {
	if name == "" {
		return "名称不能为空"
	}
	if !utf8.ValidString(name) {
		return "名称包含非法字符"
	}
	// 只由空白组成的名字等于空：调用方一般会先 TrimSpace，但这条函数是对外
	// 行为的唯一判据，不该依赖调用方"记得先 trim"。
	if strings.TrimSpace(name) == "" {
		return "名称不能只由空格组成"
	}
	if utf8.RuneCountInString(name) > maxInstanceNameRunes {
		return "名称最长 " + strconv.Itoa(maxInstanceNameRunes) + " 个字符"
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "名称不能包含控制字符或换行"
		}
		// 普通空格（U+0020）放行，其余空白一律拦下
		if c != ' ' && unicode.IsSpace(c) {
			return "名称只能包含普通空格，不能包含制表符或不换行空格"
		}
	}
	return ""
}
