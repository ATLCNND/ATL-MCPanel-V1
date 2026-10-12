package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ============================================================================
// 修改原版游戏规则（PUT /api/instances/{id}/gamerule）
// ============================================================================
//
// 为什么走控制台 `/gamerule` 而不是改 level.dat：
//
//	level.dat 里的 GameRules 只在**加载世界时**被读取，而运行中的服务器会在
//	保存世界时把它**覆写**掉。也就是说"改文件"这条路只在服务器停止时有效，
//	而且一旦服务器启动就会被抹掉 —— 这正是本仓库最忌讳的"改了却没变"。
//	`/gamerule` 是原版的运行时接口，立即生效并被服务器自己持久化。
//
//	所以：**服务器停止时本接口直接拒绝**，并说明"请先启动实例"，
//	而不是偷偷去改文件（那样会在下次启动时静默失效）。

// gameRuleNameRe 规则名的合法形态。
//
// 会拼进控制台命令，所以**必须严格**：只允许字母数字下划线（原版的规则名
// 全是这个形状，如 keepInventory / randomTickSpeed / commandBlockOutput）。
// 空格、分号、换行、$ 等一律拒绝 —— 它们能让一条"设置规则"变成任意指令。
var gameRuleNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// gameRuleValueRe 规则值的合法形态。
//
// 原版规则值只有三种形态：true/false、整数、以及极少数小数。
// 不接受空格与其它符号 —— 同样是为了防注入。
var gameRuleValueRe = regexp.MustCompile(`^(true|false|-?[0-9]{1,9})$`)

// maxGameRuleValue 数值型规则的上限。
//
// 原版对多数数值规则有自己的范围（如 randomTickSpeed 合理上限很低），
// 但**不同版本不同**，这里不做业务校验，只挡住明显荒谬的量级：
// 一个 9 位数的 randomTickSpeed 会让服务器直接卡死（那是"用一条指令打死服务器"）。
const maxGameRuleValue = 1000000

type setGameRuleReq struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// handleSetGameRule PUT /api/instances/{id}/gamerule
func (s *Server) handleSetGameRule(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")

	// 改规则 = 发控制台指令，权限与"能否在控制台发指令"一致。
	// 用 requireInstanceLevel(LevelCollab)：collab 与 owner 能发，
	// viewer 不能 —— 与 ConsoleTab 的 canSend 判据一致（见 perms.go）。
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}

	var req setGameRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	name := strings.TrimSpace(req.Name)
	value := strings.TrimSpace(req.Value)

	if !gameRuleNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest,
			"规则名不合法：只允许字母开头的字母数字组合（如 keepInventory、randomTickSpeed）")
		return
	}
	if !gameRuleValueRe.MatchString(value) {
		writeErr(w, http.StatusBadRequest,
			"规则值不合法：只允许 true / false 或整数（如 3、-1）")
		return
	}
	// 数值型额外挡一个荒谬量级（布尔值不含数字，会跳过这里）
	if value != "true" && value != "false" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			if n > maxGameRuleValue || n < -maxGameRuleValue {
				writeErr(w, http.StatusBadRequest,
					fmt.Sprintf("规则值过大（上限 %d）：这个量级会让服务器卡死", maxGameRuleValue))
				return
			}
		}
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "节点不可达："+err.Error())
		return
	}

	// 实例必须在运行：停止时 /gamerule 发不进去，改 level.dat 又会被覆写。
	// 如实拒绝比"假装成功"好 —— 见文件头的说明。
	st, err := s.instanceLiveStatus(instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st != "running" {
		writeErr(w, http.StatusConflict,
			"实例未在运行：游戏规则只能在服务器运行时修改（原版 /gamerule 是运行时指令）。"+
				"直接改存档里的规则会在服务器下次启动时被覆写，所以这里不做那种事。请先启动实例。")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	command := "gamerule " + name + " " + value
	resp, err := cli.SendCommand(ctx, &pb.CommandRequest{InstanceId: instanceID, Command: command})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "发送指令失败："+err.Error())
		return
	}
	if resp != nil && !resp.Success {
		msg := "服务器拒绝了这条指令"
		if resp.Error != "" {
			msg = resp.Error
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	s.audit(r, "set_gamerule", instanceID, name+" = "+value)

	// ⚠️ 关键：**不能**拿 level.dat 里的值去回显这次改动。
	//
	// 2026-10-05 实测发现的坑：`/gamerule` 改的是**服务器内存里的值**，
	// 只有**世界保存时**才会写进 level.dat。而「游戏设置」页的规则是解析
	// level.dat 得到的 —— 于是刚改完、界面一刷新又变回旧值，
	// 用户看到的就是"点了没生效"（实测：17:33:16 改了规则，
	// 而 level.dat 的时间戳还停在 17:32:35 那次世界保存）。
	//
	// 这不是底层没生效（控制台日志明确回显 "Gamerule X is now set to: Y"），
	// 而是**读取来源比写入来源滞后**。所以这里把"改后的真实值"直接回给前端，
	// 由前端用它就地更新显示，不让用户对着一份过期数据发懵。
	//
	// 顺带说明为什么不去"主动存盘"：原版只有 /save-all 能强制落盘，
	// 而它对一个正在跑的世界是重操作（会把所有区块写一遍），
	// 为了一次规则修改付出这个代价不划算。
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "已设置 " + name + " = " + value +
			"（已由服务器确认生效；存档里的值会在下次世界保存时落盘）",
		"name":  name,
		"value": value,
		// 提醒界面：这次改的值以本响应为准，别用 level.dat 的旧值覆盖显示
		"applied":      true,
		"persist_hint": "面板上的规则读自存档（level.dat），会在服务器下次保存世界后与这里一致",
	})
}

// instanceLiveStatus 取实例的实时状态（优先 Daemon 上报，回落面板库）。
//
// 为什么要问 Daemon：面板库里的 status 是"期望状态"，实例崩溃/被 kill 后
// 它不会自己更新（这一点在实例页也踩过，见 InstanceDetail 里的轮询说明）。
// 判断"能不能发指令"必须用实时状态，否则会给一个已经死掉的实例发指令、
// 然后报一个让人摸不着头脑的错误。
func (s *Server) instanceLiveStatus(instanceID string) (string, error) {
	var dbStatus string
	if err := s.db.QueryRow(
		`SELECT COALESCE(status, '') FROM instances WHERE instance_id = ?`, instanceID).
		Scan(&dbStatus); err != nil {
		return "", err
	}
	if cli, _, err := s.getDaemonClient(instanceID); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rt, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID}); err == nil && rt != nil {
			if rt.Status != "" {
				return rt.Status, nil
			}
		}
	}
	return dbStatus, nil
}
