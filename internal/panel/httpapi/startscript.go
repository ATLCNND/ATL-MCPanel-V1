package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// startScriptFile 实例启动脚本文件名（存在时优先于其它启动方式）。
const startScriptFile = "start.sh"

// instanceSettings instance.json 中与启动相关的字段。
type instanceSettings struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	CoreType     string `json:"core_type"`
	JavaVersion  string `json:"java_version"`
	JarPath      string `json:"jar_path"`
	MaxMem       string `json:"max_mem"`
	MinMem       string `json:"min_mem"`
	Port         int32  `json:"port"`
	StartCommand string `json:"start_command"`
}

// readInstanceSettings 读取实例元数据。
//
// 文件的**物理位置**在平台状态目录（<state_dir>/<实例ID>/instance.json，root 0700）——
// 它决定资源限制与接管行为，不能让实例用户改写，所以搬出了实例目录。
// 但路径仍按"实例内路径"请求：Daemon 那边对 instance.json 做了只读虚拟映射
// （见 grpcapi/file.go），于是这里与搬家之前完全一致，不必引入第二套数据来源。
func (s *Server) readInstanceSettings(cli pb.DaemonServiceClient, instanceID string) (instanceSettings, error) {
	var st instanceSettings
	resp, err := cli.ReadFile(context.Background(), &pb.ReadFileRequest{
		InstanceId: instanceID, Path: "instance.json",
	})
	if err != nil {
		return st, err
	}
	if !resp.Success {
		return st, fmt.Errorf("读取实例元数据失败: %s", resp.Error)
	}
	if err := json.Unmarshal([]byte(resp.Content), &st); err != nil {
		return st, fmt.Errorf("解析实例元数据失败: %w", err)
	}
	return st, nil
}

// readStartScript 读取启动脚本内容；不存在时返回 ("", false)。
func (s *Server) readStartScript(cli pb.DaemonServiceClient, instanceID string) (string, bool) {
	resp, err := cli.ReadFile(context.Background(), &pb.ReadFileRequest{
		InstanceId: instanceID, Path: startScriptFile,
	})
	if err != nil || !resp.Success {
		return "", false
	}
	return resp.Content, true
}

// renderStartCommand 按启动脚本 > 自定义命令 > 默认 java 的优先级渲染实际命令。
func renderStartCommand(st instanceSettings, hasScript bool) (command, mode string) {
	if hasScript {
		return "sh " + startScriptFile + "  （实例目录下的启动脚本）", "start.sh"
	}
	if strings.TrimSpace(st.StartCommand) != "" {
		return renderTemplate(st.StartCommand, st), "custom"
	}
	return renderTemplate(defaultStartTemplate, st), "default"
}

// defaultStartTemplate 默认启动命令模板。
const defaultStartTemplate = "{java} -Xms{min_mem} -Xmx{max_mem} -jar {jar} nogui"

// renderTemplate 替换启动命令模板中的占位符（与 Daemon 侧保持一致）。
func renderTemplate(tpl string, st instanceSettings) string {
	r := strings.NewReplacer(
		"{jar}", st.JarPath,
		"{max_mem}", normMemOr(st.MaxMem, "2G"),
		"{min_mem}", normMemOr(st.MinMem, "1G"),
		"{java}", "java",
		"{dir}", ".",
	)
	return r.Replace(tpl)
}

func normMemOr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// generateStartScript 依据当前配置生成启动脚本内容。
//
// 生成的是**可独立运行**的脚本：路径全部展开、先切到脚本所在目录，
// 并用 exec 让 java 直接接管进程（保持 PID 不变，便于面板停止与接管）。
func generateStartScript(st instanceSettings, tunnelHint bool) string {
	jar := st.JarPath
	if jar == "" {
		jar = "<未设置核心 jar，请先在「核心」页签中选择>"
	}

	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n")
	sb.WriteString("# ============================================================\n")
	sb.WriteString("# 实例启动脚本（由面板生成，可直接编辑）\n")
	sb.WriteString("#\n")
	sb.WriteString("# 生效规则：本文件存在时，面板会执行它，而忽略「自定义启动命令」与默认 java 命令。\n")
	sb.WriteString("#           删除本文件即可恢复为由面板配置的启动命令。\n")
	sb.WriteString("# 注意：修改后需在面板重启实例才会生效；本脚本通过 sh 执行，无需 chmod +x。\n")
	sb.WriteString("#\n")
	fmt.Fprintf(&sb, "# 实例: %s (%s)   核心: %s   端口: %d\n",
		st.Name, st.ID, st.CoreType, st.Port)
	fmt.Fprintf(&sb, "# 内存: -Xms%s -Xmx%s\n", normMemOr(st.MinMem, "1G"), normMemOr(st.MaxMem, "2G"))
	fmt.Fprintf(&sb, "# 生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	sb.WriteString("# ============================================================\n")
	sb.WriteString("\n")
	sb.WriteString("# 切到脚本所在目录，使相对路径（world、plugins 等）正常工作\n")
	sb.WriteString("cd \"$(dirname \"$0\")\" || exit 1\n")
	sb.WriteString("\n")
	sb.WriteString("# Java 参数：可按需调整\n")
	fmt.Fprintf(&sb, "JAVA_OPTS=\"-Xms%s -Xmx%s\"\n", normMemOr(st.MinMem, "1G"), normMemOr(st.MaxMem, "2G"))
	sb.WriteString("\n")
	sb.WriteString("# exec 让 java 直接替换当前 shell：PID 保持不变，面板的停止/接管才能正常工作\n")
	fmt.Fprintf(&sb, "exec java $JAVA_OPTS -jar \"%s\" nogui\n", jar)
	if tunnelHint {
		sb.WriteString("\n# 提示：本实例已配置公网穿透，请勿改动启动方式中的端口，否则隧道会失效。\n")
	}
	return sb.String()
}

// startScriptView 启动脚本状态视图。
type startScriptView struct {
	InstanceID  string `json:"instance_id"`
	Mode        string `json:"mode"`       // start.sh / custom / default
	ModeLabel   string `json:"mode_label"` // 中文说明
	Command     string `json:"command"`    // 实际会执行的命令
	Template    string `json:"template"`   // 自定义命令模板（可能为空）
	HasScript   bool   `json:"has_script"` // 是否存在 start.sh
	Script      string `json:"script"`     // start.sh 内容
	JarPath     string `json:"jar_path"`
	MaxMem      string `json:"max_mem"`
	MinMem      string `json:"min_mem"`
	CoreType    string `json:"core_type"`
	Editable    bool   `json:"editable"`
	GeneratedAt string `json:"generated_at"`
}

func modeLabel(mode string) string {
	switch mode {
	case "start.sh":
		return "启动脚本（start.sh）"
	case "custom":
		return "自定义启动命令"
	default:
		return "默认 java 命令"
	}
}

// handleGetStartScript GET /api/instances/{id}/start-script
func (s *Server) handleGetStartScript(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	st, err := s.readInstanceSettings(cli, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	script, hasScript := s.readStartScript(cli, instanceID)
	command, mode := renderStartCommand(st, hasScript)

	writeJSON(w, http.StatusOK, startScriptView{
		InstanceID:  instanceID,
		Mode:        mode,
		ModeLabel:   modeLabel(mode),
		Command:     command,
		Template:    st.StartCommand,
		HasScript:   hasScript,
		Script:      script,
		JarPath:     st.JarPath,
		MaxMem:      st.MaxMem,
		MinMem:      st.MinMem,
		CoreType:    st.CoreType,
		Editable:    true,
		GeneratedAt: time.Now().Format(time.RFC3339),
	})
}

// handleSetStartScript POST /api/instances/{id}/start-script
//
// body: {"action":"generate"|"save"|"remove", "content":"..."}
//   - generate: 依据当前实例配置生成 start.sh
//   - save:     保存自定义内容为 start.sh
//   - remove:   删除 start.sh，恢复使用面板配置的启动命令
func (s *Server) handleSetStartScript(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Action  string `json:"action"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	switch req.Action {
	case "remove":
		resp, err := cli.DeleteFile(ctx, &pb.FileRequest{InstanceId: instanceID, Path: startScriptFile})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !resp.Success {
			// 文件本就不存在时视为成功
			if !strings.Contains(resp.Error, "no such file") {
				writeErr(w, http.StatusInternalServerError, resp.Error)
				return
			}
		}
		s.audit(r, "remove_start_script", instanceID, "")
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "已删除启动脚本，实例将使用面板中配置的启动命令（重启后生效）",
		})

	case "save", "generate":
		content := req.Content
		if req.Action == "generate" {
			st, err := s.readInstanceSettings(cli, instanceID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			content = generateStartScript(st, false)
		}
		if strings.TrimSpace(content) == "" {
			writeErr(w, http.StatusBadRequest, "脚本内容不能为空")
			return
		}

		resp, err := cli.WriteFile(ctx, &pb.WriteFileRequest{
			InstanceId: instanceID, Path: startScriptFile, Content: content,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !resp.Success {
			writeErr(w, http.StatusInternalServerError, resp.Error)
			return
		}
		s.audit(r, "save_start_script", instanceID, fmt.Sprintf("%d 字节", len(content)))
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "启动脚本已保存（重启实例后生效）",
		})

	default:
		writeErr(w, http.StatusBadRequest, "action 必须是 generate / save / remove")
	}
}
