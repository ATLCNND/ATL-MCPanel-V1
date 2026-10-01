package analysis

import (
	"fmt"
	"strings"
)

// 求助模板：**面板预先写好一段可以直接贴出去的求助帖**，管理员可在界面上改。
//
// 为什么做成模板而不是继续写死在代码里（2026-09-30 用户反馈）：
//
//	原来"求助文本"是 HelpText() 里一串硬编码字符串。用户看到的是"现象"输入框空着、
//	生成出来的段落也没法调 —— 但不同社区要的信息其实不一样（有的要贴 crash-report 全文，
//	有的要 mods 列表，有的群规要求先写"已尝试过什么"）。把它变成**可编辑模板**，
//	面板只负责把占位符替换成真实环境信息，其余措辞归管理员。
//
// 占位符写在一对花括号里（{url} 这种）。规则刻意做得很小：
//   - 只做**整词替换**，不做条件/循环 —— 模板要能被非程序员看懂；
//   - **某一行上的占位符全部为空时，整行丢掉** —— 否则会留下 "核心：" 这种悬空标签；
//   - {url} 是唯一必填的占位符（没有链接的"求助帖"没有意义），保存时会校验。
//
// 没写成 text/template 是因为：那边 {{if}} 之类的语法一旦开放，模板就成了代码，
// 出错只能报一行 Go 的错误信息给管理员看，比不会用更糟。
const DefaultHelpTemplate = `【求助】Minecraft 服务器崩溃 / 报错（{instance}）

日志：{url}
原文：{raw_url}
日志里数出 {errors} 行 ERROR。

运行环境（这些在日志里通常看不到）：
· 核心：{core}
· Java：{java}
· 内存：{mem}
· 运行方式：{runtime}

现象 / 已经试过什么：
{phenomenon}

（本段由 {panel} 自动生成，可直接粘贴到社区或群里求助）`

// HelpVars 渲染模板时可用的值。
//
// Errors 用字符串而不是 int：**空串表示"没有 ERROR 行"**，那一行会被整行丢掉
// （写 "0 行 ERROR" 只会让帮你的人以为你看错了日志）。
type HelpVars struct {
	Instance   string
	Panel      string
	URL        string
	RawURL     string
	Errors     string
	Core       string
	Java       string
	Mem        string
	Runtime    string
	Phenomenon string
}

// helpPlaceholderDocs 占位符说明（界面上的图例由它生成，避免文档与实现走散）。
type HelpPlaceholder struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

var helpPlaceholderDocs = []HelpPlaceholder{
	{"instance", "实例名（如 内测-Beta01）"},
	{"panel", "面板名称（{panel} 会写成你的面板名）"},
	{"url", "日志分享链接（**必填**：模板里必须有它）"},
	{"raw_url", "日志原文直链（社区里想直接看纯文本的人用得上）"},
	{"errors", "日志里 ERROR 的行数（为 0 时这一行会整行消失）"},
	{"core", "服务端核心与版本（如 paper 1.21.1）"},
	{"java", "Java 版本（如 21）"},
	{"mem", "实例内存上限（容器化时会带上 cgroup 上限）"},
	{"runtime", "运行方式（容器化 / 普通进程）"},
	{"phenomenon", "用户自己填的现象描述（没填则这一行消失）"},
}

// HelpPlaceholders 返回占位符图例（界面用）。
func HelpPlaceholders() []HelpPlaceholder { return helpPlaceholderDocs }

// RenderHelp 按模板生成求助文本。
//
// tmpl 为空则用内置默认模板（管理员把模板清空 = 恢复默认，而不是"生成空文本"）。
func RenderHelp(tmpl string, v HelpVars) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = DefaultHelpTemplate
	}
	vals := map[string]string{
		"instance":   v.Instance,
		"panel":      v.Panel,
		"url":        v.URL,
		"raw_url":    v.RawURL,
		"errors":     v.Errors,
		"core":       v.Core,
		"java":       v.Java,
		"mem":        v.Mem,
		"runtime":    v.Runtime,
		"phenomenon": strings.TrimSpace(v.Phenomenon),
	}
	if vals["panel"] == "" {
		vals["panel"] = "ATL-MCPanel"
	}

	lines := strings.Split(strings.ReplaceAll(tmpl, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		rendered, allEmpty := renderLine(line, vals)
		// 整行都是空占位符 → 丢掉这一行（连同行内的标签与 · 之类的装饰）
		if allEmpty {
			continue
		}
		out = append(out, strings.TrimRight(rendered, " \t"))
	}
	text := strings.Join(out, "\n")
	// 连续空行压成一个：模板里为了可读性留的空行，遇到"整行消失"后容易叠起来
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

// renderLine 替换一行里的占位符。
//
// 返回 allEmpty=true 表示：这一行**含占位符**，且所有占位符的值都是空的 —— 调用方丢掉整行。
// 不含占位符的行永远保留（那是管理员自己写的固定文案）。
func renderLine(line string, vals map[string]string) (string, bool) {
	var (
		b        strings.Builder
		rest     = line
		hadPH    bool
		hadValue bool
	)
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			b.WriteString(rest)
			break
		}
		close := strings.Index(rest[open:], "}")
		if close < 0 {
			b.WriteString(rest)
			break
		}
		close += open
		name := rest[open+1 : close]
		val, known := vals[name]
		if !known {
			// 未知占位符原样保留：管理员写错名字时能自己看见，而不是被静默吞掉
			b.WriteString(rest[:close+1])
			rest = rest[close+1:]
			continue
		}
		hadPH = true
		if strings.TrimSpace(val) != "" {
			hadValue = true
		}
		b.WriteString(rest[:open])
		b.WriteString(val)
		rest = rest[close+1:]
	}
	return b.String(), hadPH && !hadValue
}

// ValidateHelpTemplate 保存前校验（错误信息要直接告诉管理员怎么改）。
func ValidateHelpTemplate(tmpl string) error {
	t := strings.TrimSpace(tmpl)
	if t == "" {
		return nil // 空 = 恢复默认
	}
	if len(t) > 4000 {
		return fmt.Errorf("模板过长（%d 字，上限 4000 字）", len([]rune(t)))
	}
	if !strings.Contains(t, "{url}") {
		return fmt.Errorf("模板里必须保留 {url} 占位符 —— 没有日志链接，别人无法帮你排查")
	}
	return nil
}

// HelpText 兼容旧调用点：用内置默认模板渲染。
//
// 保留这个签名是为了让**已有测试与老接口**继续可用；新的调用点应该走 RenderHelp +
// 面板设置里的模板（见 httpapi.helpTextFor）。
func HelpText(instanceName, coreType, javaVersion, memLimit, containerNote, phenomenon, url, raw string, errors int) string {
	errStr := ""
	if errors > 0 {
		errStr = fmt.Sprintf("%d", errors)
	}
	return RenderHelp(DefaultHelpTemplate, HelpVars{
		Instance: instanceName, URL: url, RawURL: raw, Errors: errStr,
		Core: coreType, Java: javaVersion, Mem: memLimit, Runtime: containerNote,
		Phenomenon: phenomenon,
	})
}
