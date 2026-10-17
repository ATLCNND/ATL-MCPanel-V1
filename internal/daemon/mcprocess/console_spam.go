package mcprocess

import (
	"regexp"
	"strings"
)

// SpamRule 客户端下发的「周期性刷屏」规则。
//
// 为什么规则由客户端下发，而不是在 Daemon 里自己维护一套：
// 判定"什么算刷屏"的规则（预制规则 + 用户自定义正则）现在只活在前端
// （web/src/components/ConsoleTab.tsx 的 HEARTBEAT_PRESETS 与自定义规则），
// 用户在界面上能逐条开关、也能自己加。Daemon 若另写一份判定，两边迟早走偏 ——
// 那正是 internal/panel/httpapi/console.go 里明确要避免的"第二份实现"。
// 所以前端把**当前生效的规则集**随附加请求一起送下来，Daemon 用它来数"回放多少行"，
// 于是用户在界面上屏蔽的刷屏，也不会再占掉那 500 行回放窗口的额度。
//
// Pattern 是 JS 正则的 source（不含两侧斜杠），Flags 只认 i / m / s 三个
// （其余如 g / y / u 对"逐行匹配"没有意义，忽略即可）。
type SpamRule struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Flags   string `json:"flags"`
	// Block 块状规则：命中后不只是这一行，从它开始的整个多行块都算刷屏
	//（Folia 的 Server Health Report 就是十几行的块）。
	Block bool `json:"block"`
}

// 规则数量与长度上限：规则来自浏览器（经面板校验后转发），属于外部输入。
// 超限的规则直接丢弃（保守方向：判不出刷屏就多留行，不会误删）。
const (
	maxSpamRules        = 64
	maxSpamPatternBytes = 512
)

type compiledSpamRule struct {
	id    string
	re    *regexp.Regexp
	block bool
}

// SpamFilter 编译好的刷屏规则集。
type SpamFilter struct {
	rules []compiledSpamRule
	// blockTails 至少能跳过几行（块内续行上限）。设一个有限值，
	// 免得一条误配的块规则把后面所有行都吞掉。
	blockTail int
}

// 块内最多连带跳过多少行。Folia 的健康报告实测十几行，40 行留了充足余量。
const spamBlockMaxTail = 40

// NewSpamFilter 编译客户端下发的规则。
//
// 返回被**跳过**的规则 id：Go 的 regexp 是 RE2，不支持 lookahead/lookbehind 等
// 语法（JS 支持），那类自定义规则编译不过。跳过是保守方向 —— 判不出刷屏就多留行，
// 绝不会把用户想看的行删掉；调用方（Daemon 的 Console 处理）会把跳过的 id 记进日志，
// 免得用户对着"规则没生效"百思不得其解。
func NewSpamFilter(rules []SpamRule) (*SpamFilter, []string) {
	if len(rules) == 0 {
		return nil, nil
	}
	f := &SpamFilter{blockTail: spamBlockMaxTail}
	var skipped []string
	for _, r := range rules {
		if len(f.rules) >= maxSpamRules || len(r.Pattern) == 0 || len(r.Pattern) > maxSpamPatternBytes {
			if r.ID != "" || r.Pattern != "" {
				skipped = append(skipped, r.ID)
			}
			continue
		}
		pat := goRegexSource(r.Pattern, r.Flags)
		re, err := regexp.Compile(pat)
		if err != nil {
			skipped = append(skipped, r.ID)
			continue
		}
		f.rules = append(f.rules, compiledSpamRule{id: r.ID, re: re, block: r.Block})
	}
	if len(f.rules) == 0 {
		return nil, skipped
	}
	return f, skipped
}

// goRegexSource 把 JS 的标志位翻译成 Go 的内联标志。
//
// 只翻译对逐行匹配有意义的三个：i（忽略大小写）、m（^ $ 匹配行首行尾）、s（. 匹配换行）。
// 其它标志（g / y / u / d）不翻译：g 与 y 是"多次/黏性匹配"的调用约定，u 在 RE2 里是默认行为。
func goRegexSource(pattern, flags string) string {
	var on []byte
	for _, c := range flags {
		switch c {
		case 'i', 'm', 's':
			if !strings.ContainsRune(string(on), c) {
				on = append(on, byte(c))
			}
		}
	}
	if len(on) == 0 {
		return pattern
	}
	return "(?" + string(on) + ")" + pattern
}

// IsSpam 这一行是否属于"周期性刷屏"。
func (f *SpamFilter) IsSpam(line string) bool {
	if f == nil {
		return false
	}
	for _, r := range f.rules {
		if r.re.MatchString(line) {
			return true
		}
	}
	return false
}

// matchBlock 这一行是否命中某条**块状**规则。
func (f *SpamFilter) matchBlock(line string) bool {
	if f == nil {
		return false
	}
	for _, r := range f.rules {
		if r.block && r.re.MatchString(line) {
			return true
		}
	}
	return false
}

// newEntryRe 判断一行是不是"一条新的日志记录"（形如 `[12:34:56 INFO]: ...`）。
//
// 用途：块状规则命中后要连后续续行一起跳过，而"块什么时候结束"只能靠这个判断 ——
// 续行（` - Online Players: 0`、`Chunks: 529`）没有时间戳前缀，下一条真正的日志行才有。
var newEntryRe = regexp.MustCompile(`^\s*\[?\s*\d{1,2}:\d{2}:\d{2}`)

// ansiRe 去掉颜色转义（`\x1b[90m` 之类），判断形状时不该受它干扰。
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// UsefulTail 从若干行里取**尾部 maxLines 条"非刷屏"行**。
//
// 块状规则必须**自前往后**判定（块头决定后续若干行算不算续行），所以这里先整体
// 标注一遍（O(n)），再从后往前取够数的那几条 —— 而不是从后往前边取边判。
func (f *SpamFilter) UsefulTail(lines []string, maxLines int) []string {
	if maxLines <= 0 || len(lines) == 0 {
		return nil
	}
	if f == nil || len(f.rules) == 0 {
		return lastN(lines, maxLines)
	}

	keep := make([]bool, len(lines))
	blockTail := 0
	for idx, ln := range lines {
		if blockTail > 0 {
			if !newEntryRe.MatchString(ansiRe.ReplaceAllString(ln, "")) {
				blockTail-- // 仍是块内续行
				continue
			}
			blockTail = 0
		}
		if f.IsSpam(ln) {
			if f.matchBlock(ln) {
				blockTail = f.blockTail
			}
			continue
		}
		keep[idx] = true
	}

	out := make([]string, 0, maxLines)
	for idx := len(lines) - 1; idx >= 0 && len(out) < maxLines; idx-- {
		if keep[idx] {
			out = append(out, lines[idx])
		}
	}
	// 上面是从后往前收集的，翻回正常顺序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// lastN 取尾部 n 行（不带过滤时的行为，与改动前一致）。
func lastN(lines []string, n int) []string {
	if n <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}
