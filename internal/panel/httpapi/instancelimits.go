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
// 实例资源上限的修改（PUT /api/instances/{id}/limits）
// ============================================================================
//
// 背景：这三个上限（cpu_quota / mem_limit / disk_limit_mb）此前**只在建实例时**
// 写一次 —— 全仓库没有任何地方 UPDATE 过它们。于是"节点用户建实例时把内存上限
// 留空（= 不限制）"就成了运营侧永远收不回来的口子：实例跑了大半年、想把内存
// 压回 4G，只能删库重建。这个接口补的就是这一环。
//
// 谁能改：与"改名 / 公网端口"**完全同一套判据**（canManageInstanceSettings）——
// 总管理员、该实例所在节点的节点用户、以及该实例的 owner 本人。
//
//   - 为什么 owner 也算在内：这三个值本来就是"创建者自己声明"的
//     （见 handleCreateInstance 的 createInstanceReq），实例归属者对自己的机器
//     多要一个核、少要一点内存，都是自己承担后果的决定；反过来"我的实例我却
//     不能给自己加内存"显然说不通。
//   - 为什么 collab / viewer 不算：资源上限会挤占**同节点上其它实例**
//     （CPU 抢满时最先受害的是邻居），这不是"被授权看控制台的人"该决定的事。
//     审计里逐字段记了"旧值 → 新值"，正是为了让运营侧能查出"是谁把它放开的"。
//
// 生效时机（**必须如实告诉用户**，这个仓库最忌讳"改了却没变"）：
//
//   - disk_limit_mb：面板自己的调度器每分钟巡检一次配额并分级告警
//     （见 scheduler 里的 disk_limit_mb 查询），改完下一个巡检周期就生效，
//     **不需要重启** —— 这条链路上没有 Daemon 的事。
//   - cpu_quota / mem_limit：对应的 cgroup 文件（cpu.max / memory.max）
//     由 Daemon 在**实例启动时**写入（见 daemon/mcprocess.process.go 的
//     Apply / SetMemoryLimit）。运行中的进程改不了自己脚下的配额，
//     所以对运行中的实例只能是"重启后生效"——与「启动」页换 JDK 的行为一致，
//     响应与界面都会明说这一点。
//
// 节点侧同步（2026-10-02 补上，原先是个缺口）：
//
//	Daemon 判定资源限制的依据是**节点上的 instance.json**（registry.Meta 的
//	CPUQuota / MemLimit），而它原先只在建实例时被 CreateInstance 写入 ——
//	只改面板库的话，界面上写着 4G、cgroup 里还是 unlimited，属于最典型的
//	"改了却没变"。所以现在更新完库之后会**调用 SetInstanceLimits** 把新值
//	同步到节点（该 RPC 只落节点元数据，真正施加仍在下次启动）。
//	节点不可达时不清空库里的值，而是在响应里明确告知"尚未同步到节点"。
func (s *Server) handleSetInstanceLimits(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")

	if !s.canManageInstanceSettings(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "无权修改该实例的资源限制（需要实例所有者或节点管理员）")
		return
	}

	var req instanceLimitsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if msg := checkInstanceLimits(&req); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	// 旧值与状态一起读出来：审计里要留下"谁把哪个上限从多少改成了多少"
	//（运营侧最需要能查出"是谁把内存放成不限制的"），顺带充当存在性检查。
	var (
		oldCPU  int
		oldMem  string
		oldDisk int64
		status  string
	)
	if err := s.db.QueryRow(
		`SELECT cpu_quota, COALESCE(mem_limit, ''), COALESCE(disk_limit_mb, 0), status
		 FROM instances WHERE instance_id = ?`, instanceID).
		Scan(&oldCPU, &oldMem, &oldDisk, &status); err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	// 部分更新：只有**出现在请求体里**的字段才进 SET。
	//
	// 列名片段是固定字符串、值一律走占位符，与 handleUpdateNode 的写法一致 ——
	// 拼进 SQL 的永远是代码里写死的列名，用户输入没有任何机会碰到它。
	sets := []string{}
	args := []interface{}{}
	var changes []string
	newCPU, newMem, newDisk := oldCPU, oldMem, oldDisk

	if req.CPUQuota != nil {
		v := int(*req.CPUQuota)
		sets = append(sets, "cpu_quota = ?")
		args = append(args, v)
		if v != oldCPU {
			changes = append(changes, "CPU 配额 "+cpuQuotaText(oldCPU)+" → "+cpuQuotaText(v))
			newCPU = v
		}
	}
	if req.MemLimit != nil {
		v := *req.MemLimit
		sets = append(sets, "mem_limit = ?")
		args = append(args, v)
		if v != oldMem {
			changes = append(changes, "内存上限 "+memLimitText(oldMem)+" → "+memLimitText(v))
			newMem = v
		}
	}
	if req.DiskLimitMB != nil {
		v := *req.DiskLimitMB
		sets = append(sets, "disk_limit_mb = ?")
		args = append(args, v)
		if v != oldDisk {
			changes = append(changes, "磁盘配额 "+diskLimitText(oldDisk)+" → "+diskLimitText(v))
			newDisk = v
		}
	}
	if len(sets) == 0 {
		writeErr(w, http.StatusBadRequest,
			"没有需要更新的字段（cpu_quota / mem_limit / disk_limit_mb 至少带一个）")
		return
	}

	// 带是带了、值却没变（用户点了两次保存）：不写库、不记审计 ——
	// 否则审计里会被这种空操作刷满，反而盖住真正的改动。
	// 与 handleRenameInstance 的"名称未变化"是同一个处理思路。
	if len(changes) == 0 {
		writeInstanceLimits(w, newCPU, newMem, newDisk, false, "资源上限没有变化", "")
		return
	}

	args = append(args, instanceID)
	if _, err := s.db.Exec(
		`UPDATE instances SET `+strings.Join(sets, ", ")+` WHERE instance_id = ?`, args...); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 审计：旧值 → 新值，只记真的动过的那几项（翻日志时一行就能看全）
	s.audit(r, "update_instance_limits", instanceID, strings.Join(changes, " · "))

	// 生效时机由后端说清楚，界面直接显示这句话、不再自己编文案 ——
	// 免得以后有人只改了后端、界面上的说明却留成旧口径。
	cpuChanged := req.CPUQuota != nil && newCPU != oldCPU
	memChanged := req.MemLimit != nil && newMem != oldMem
	diskChanged := req.DiskLimitMB != nil && newDisk != oldDisk
	restartRequired := cpuChanged || memChanged

	msg := "已保存资源上限：CPU " + cpuQuotaText(newCPU) +
		" · 内存 " + memLimitText(newMem) +
		" · 磁盘 " + diskLimitText(newDisk)
	note := ""

	// CPU / 内存上限的真正施加方是 Daemon（启动时写 cgroup），而它的取值来源是
	// **节点上的 instance.json** —— 所以改完面板库还不够，必须把新值同步到节点，
	// 否则就是"界面上写着 4G、cgroup 里还是 unlimited"。
	//
	// 同步是**全量覆盖**（两个字段一起发）：只发改动过的那个会让"另一个字段"
	// 的语义变成"未指定"，而 proto3 无法区分"没传"和"传了 0（不限制）"——
	// 那种歧义正是"顺手把内存限额清掉"这类事故的来源。所以这里把**最终想要的值**
	// 一起发过去（部分更新已经在上面用旧值补齐了）。
	if restartRequired {
		if cli, _, err := s.getDaemonClient(instanceID); err != nil {
			// 节点连不上：面板的值已经落库，但**没同步到节点**。如实说，
			// 不要让用户以为已经生效（"改了却没变"是本仓库最忌讳的一类事故）。
			note = "节点暂时不可达，CPU / 内存上限**尚未同步到节点**：" + err.Error() +
				"；面板已记下新值，请在节点恢复后重新保存一次。"
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			resp, err := cli.SetInstanceLimits(ctx, &pb.SetInstanceLimitsRequest{
				InstanceId: instanceID,
				CpuQuota:   int32(newCPU),
				MemLimit:   newMem,
			})
			switch {
			case err != nil:
				note = "已同步到节点，但调用返回错误（节点可能未应用）：" + err.Error() +
					"；可稍后重新保存一次。"
			case resp != nil && !resp.Success:
				note = "节点拒绝了这次修改：" + resp.Error
			default:
				// 同步成功：不再需要"尚未生效"那句丑话，节点侧元数据已经是新值了
				note = ""
			}
		}
		if status == "running" {
			msg += "。实例正在运行：CPU / 内存上限由 Daemon 在实例启动时写入 cgroup，" +
				"需要重启实例后生效（当前进程仍按启动时的那个值运行）"
		} else {
			msg += "。CPU / 内存上限已同步到节点，将在下次启动实例时生效"
		}
	}
	if diskChanged {
		msg += "。磁盘配额由面板每分钟巡检一次，下一个巡检周期即生效（不需要重启）"
	}

	writeInstanceLimits(w, newCPU, newMem, newDisk, restartRequired, msg, note)
}

// instanceLimitsReq PUT /api/instances/{id}/limits 的请求体。
//
// 三个字段都用**指针**：必须能区分"没传这个字段"与"显式设成 0 / 空串"。
//
//	0（cpu_quota）、空串（mem_limit）、0（disk_limit_mb）都是**有意义的取值**
//	——它们都表示"不限制"。用值类型的话，一个只想改磁盘配额的请求（body 里
//	根本没有 cpu_quota）会被解码成 cpu_quota=0，顺手把那台实例的 CPU 配额
//	抹成不限制：这是部分更新最典型的静默事故。分析页的 filter_chat 用的是
//	同一个套路（见 analysis_runs.go 里对 *bool 的说明）。
type instanceLimitsReq struct {
	CPUQuota    *int32  `json:"cpu_quota"`     // CPU 配额百分比（100 = 1 核；0 = 不限制）
	MemLimit    *string `json:"mem_limit"`     // cgroup 内存上限（如 "4G"；空 = 不限制）
	DiskLimitMB *int64  `json:"disk_limit_mb"` // 实例目录软配额（MB，0 = 不限制）
}

// maxCPUQuotaPct CPU 配额上限（百分比，3200 = 32 核）。
//
// 取这个数的理由：它会被 Daemon 换算成 cgroup v2 的 cpu.max
//（"<quota> <period>"，见 daemon/cgroup/v2.go）原样施加。没有上限的话，
// 一次手滑（想填 100 却敲成 10000）得到的不是报错，而是一份语义荒谬的
// cgroup 文件：看着"配额很大"，实际是允许这台实例吃满 100 核 —— 在 8/16 核的
// 节点上它会和所有邻居抢 CPU，表现成"别人莫名其妙掉 TPS"，而且从面板上
// 完全看不出原因。32 核本身也已远超单个 MC 实例的收益上限（主逻辑 tick 是
// 单线程的，多出来的配额只对区块生成与 GC 有边际作用）。
const maxCPUQuotaPct = 3200

// maxDiskLimitMB 磁盘配额上限（MB，即 1 TiB）。
//
// 它只是面板巡检用的软配额、不写进内核，但同样需要上界：没有它，把单位记错
//（想给 50G 却填 50，或者反过来填成 51200000）时不会报错，只留下一个
// 永远触发不了的配额。真要"不限制"，这个字段的 0 就是明确表达。
const maxDiskLimitMB = 1024 * 1024

// maxMemLimitBytes 内存上限的数值上界（1 TiB，按字节算）。
//
// mem_limit 会被 Daemon 以 root 写进 memory.max，所以除了字面量格式，
// 数值本身也要有个"荒诞值挡板"：写成 4096G 或一长串数字时，用户真正想表达的
// 其实是"不限制"—— 那种情况下**留空**才是这个字段的正确写法（见建实例的 INSERT，
// mem_limit 空串就是不限制），而不是填一个永远碰不到的天文数字。
const maxMemLimitBytes = 1 << 40

// memLimitRe 合法的内存上限字面量。
//
// 这个字符串会被 Daemon **原样写进节点的 cgroup 文件 memory.max**，
// 所以只接受内核解析器眼里无歧义的一整串"数字 + 可选单位"：
//
//   - 512M / 4G / 1536m（大小写都收）；
//   - 纯字节数 1073741824（不给单位就是字节）；
//   - 单位后多一个 B（512MB），以及单独的 B（512B）—— 内核 memparse
//     会把结尾的 B 吃掉。
//
// 刻意**不接受**的东西，每一条都是"它会被写进 cgroup 文件"推出来的：
// 空格与第二个 token（"4G 8G"）、分号/换行（"4G;rm -rf /"）、路径片段
//（"../../x"）、负号、小数点（"1.5G" —— 内核不解析小数，会直接写失败）、
// "1GiB"（不认 i 后缀）、以及超出 int64 的天文数字。
//
// 另注：Daemon 侧的 mcprocess.ParseMemBytes 原先对**不带单位**的纯数字按 MB 算
//（内核与 docker 都按字节，差 1M 倍），并且会把 "512MB" 解析失败后当成 0
//（= 不限制）—— 也就是说面板放行的这两种写法在节点上会静默变形。
// 已于 2026-10-02 一并修掉（见 process.go 里 ParseMemBytes 的注释），
// 现在两边对同一串字面量的解读一致：不带单位 = 字节，结尾 B 只表示单位结束。
var memLimitRe = regexp.MustCompile(`^[0-9]+([kKmMgGtT][bB]?|[bB])?$`)

// checkInstanceLimits 校验三个上限，返回空字符串表示通过。
//
// 分三段写是因为每个字段"合法"的边界由它落地的地方决定，三者互不相同：
// cpu_quota → cgroup cpu.max 的配额；mem_limit → memory.max 的整行内容；
// disk_limit_mb → 只进面板巡检的算术。
func checkInstanceLimits(req *instanceLimitsReq) string {
	if req.CPUQuota != nil {
		v := *req.CPUQuota
		if v < 0 || v > maxCPUQuotaPct {
			return fmt.Sprintf("CPU 配额需为 0 ~ %d 之间的整数（百分比，100 = 1 核；0 = 不限制）", maxCPUQuotaPct)
		}
	}
	if req.MemLimit != nil {
		v := *req.MemLimit
		// 空串 = 不限制，这是这个字段本来就有的语义，不算"没填"
		if v != "" {
			// 刻意**不 TrimSpace**：带空格的字面量写进 cgroup 文件就是内核要拒绝的
			// 东西，与其在这里悄悄修好它（然后用户在别处照着写就不灵了），
			// 不如把规则说清楚。前端输入框自己会 trim，正常路径碰不到这条。
			if !memLimitRe.MatchString(v) {
				return "内存上限格式不合法：只能是数字加单位（如 512M、4G、1536m）或纯字节数；留空 = 不限制"
			}
			if !memLimitValueOK(v) {
				return fmt.Sprintf("内存上限过大（最大 %dG）；要表示不限制请留空", maxMemLimitBytes>>30)
			}
		}
	}
	if req.DiskLimitMB != nil {
		v := *req.DiskLimitMB
		if v < 0 || v > maxDiskLimitMB {
			return fmt.Sprintf("磁盘配额需为 0 ~ %d 之间的整数（MB；0 = 不限制）", maxDiskLimitMB)
		}
	}
	return ""
}

// memLimitValueOK 校验内存上限字面量的**数值**部分（格式已由 memLimitRe 把关）。
//
// 为什么还要单独算一遍：格式正确不代表数值合理 —— "4096G"、"999999999999999"
// 都是"格式没问题、却根本没打算真的生效"的写法。同时这也是溢出防线：
// 先拿数字跟"上界÷单位"比、再乘单位，就不会出现"乘完绕成负数，
// 于是一个天文数字反而被判成很小的限额"这种事。
func memLimitValueOK(v string) bool {
	// 结尾的 B/b（512MB / 512B）先摘掉：它只表示"单位到此结束"，不改变数量级
	if n := len(v); n > 0 && (v[n-1] == 'b' || v[n-1] == 'B') {
		v = v[:n-1]
	}
	mul := int64(1)
	if n := len(v); n > 0 {
		switch v[n-1] {
		case 'k', 'K':
			mul = 1 << 10
		case 'm', 'M':
			mul = 1 << 20
		case 'g', 'G':
			mul = 1 << 30
		case 't', 'T':
			mul = 1 << 40
		}
		if mul > 1 {
			v = v[:n-1]
		}
	}
	num, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// 位数多到 int64 装不下：只可能是随手敲的一串数字
		return false
	}
	return num <= maxMemLimitBytes/mul
}

// writeInstanceLimits 组装响应。
//
// 把改后的三个值一并回带：界面保存完可以直接回显，不必再拉一次实例列表；
// restart_required 是给界面用的开关（决定要不要把"重启后生效"那句提示挂出来）——
// 光靠 message 里的一句话判断会让前端去猜文案，那正是"改了却没变"的温床。
//
// note 现在是**错误通道**：正常路径为空串，只有"节点不可达 / 节点拒绝了这次修改"
// 才会有内容（2026-10-02 接通节点侧同步之前，它承载的是"尚未真正生效"那句丑话）。
func writeInstanceLimits(w http.ResponseWriter, cpu int, mem string, disk int64, restartRequired bool, msg, note string) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":          msg,
		"note":             note,
		"cpu_quota":        cpu,
		"mem_limit":        mem,
		"disk_limit_mb":    disk,
		"restart_required": restartRequired,
	})
}

// cpuQuotaText CPU 配额的可读形式（100 = 1 核；0 = 不限制）。
// 允许半核，所以不能直接取整 —— 与前端 cores = cpu_quota / 100 的换算一致。
func cpuQuotaText(pct int) string {
	if pct <= 0 {
		return "不限制"
	}
	cores := float64(pct) / 100
	if cores == float64(int(cores)) {
		return fmt.Sprintf("%d 核(%d%%)", int(cores), pct)
	}
	return fmt.Sprintf("%.1f 核(%d%%)", cores, pct)
}

// memLimitText 内存上限的可读形式（空 = 不限制）。
func memLimitText(v string) string {
	if v == "" {
		return "不限制"
	}
	return v
}

// diskLimitText 磁盘配额的可读形式（0 = 不限制）。
func diskLimitText(mb int64) string {
	if mb <= 0 {
		return "不限制"
	}
	return strconv.FormatInt(mb, 10) + " MB"
}
