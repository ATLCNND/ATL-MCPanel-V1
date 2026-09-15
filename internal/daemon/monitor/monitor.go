// Package monitor 采集 Minecraft 实例的运行时数据（TPS、在线玩家）。
//
// 实现方式（不依赖 RCON）：
//   - 订阅实例控制台输出，解析玩家的 joined/left 事件与 list/tps 命令响应；
//   - 周期性向托管实例的 stdin 注入 list / tps 命令以刷新基线；
//   - 结果写入 Store 供 gRPC 监控接口合并输出。
//
// 说明：当前 Folia 版本的 RCON 无法执行命令（连 help 都返回
// "Error executing: xxx (null)"），因此不走 RCON。
package monitor

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
)

// Stats 单个实例的运行时数据。
type Stats struct {
	TPS        float64
	Players    int32
	MaxPlayers int32
	Updated    time.Time
	Note       string // 采集说明（异常时填写）
}

// Store 线程安全的统计数据仓库。
type Store struct {
	mu    sync.RWMutex
	stats map[string]Stats
}

// NewStore 创建仓库。
func NewStore() *Store {
	return &Store{stats: make(map[string]Stats)}
}

// Set 写入统计数据。
func (s *Store) Set(instanceID string, st Stats) {
	s.mu.Lock()
	s.stats[instanceID] = st
	s.mu.Unlock()
}

// Get 读取统计数据。
func (s *Store) Get(instanceID string) (Stats, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.stats[instanceID]
	return st, ok
}

// Forget 删除统计数据。
func (s *Store) Forget(instanceID string) {
	s.mu.Lock()
	delete(s.stats, instanceID)
	s.mu.Unlock()
}

// ---- 日志解析 ----

var (
	colorCodeRe = regexp.MustCompile("§.")

	// "There are 0 of a max of 20 players online: a, b"
	listRe = regexp.MustCompile(`There are (\d+) of a max of (\d+) players online`)

	// "[19:08:55 INFO]: Steve joined the game"
	joinRe = regexp.MustCompile(`]:\s+(\S+) joined the game`)
	leftRe = regexp.MustCompile(`]:\s+(\S+) left the game`)

	// Paper: "TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0"
	// Folia:  " - Median Region TPS: 18.96"（另有 Lowest/Highest Region TPS）
	tpsPaperRe = regexp.MustCompile(`TPS from last [^:]*:\s*([0-9]+(?:\.[0-9]+)?)`)
	tpsFoliaRe = regexp.MustCompile(`Median Region TPS:\s*([0-9]+(?:\.[0-9]+)?)`)
	tpsFoliaLo = regexp.MustCompile(`Lowest Region TPS:\s*([0-9]+(?:\.[0-9]+)?)`)
)

// instanceState 单个实例的采集状态。
type instanceState struct {
	stop chan struct{}

	mu         sync.Mutex
	players    map[string]struct{} // 在线玩家名
	maxPlayers int32
	tps        float64
	note       string
	tpsProbed  bool // 是否已判定 tps 命令是否可用
	tpsOK      bool
}

// Poller 采集器。
type Poller struct {
	reg   *registry.Registry
	store *Store
	log   *logger.Logger

	mu      sync.Mutex
	states  map[string]*instanceState
	stopped bool
}

// NewPoller 创建采集器。
func NewPoller(reg *registry.Registry, store *Store, log *logger.Logger) *Poller {
	return &Poller{
		reg:    reg,
		store:  store,
		log:    log,
		states: make(map[string]*instanceState),
	}
}

// 命令注入间隔：较低频，避免污染控制台输出
const (
	injectInterval = 60 * time.Second
	tickInterval   = 3 * time.Second
)

// Run 启动采集循环（阻塞，应在 goroutine 中调用）。
func (p *Poller) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	lastInject := make(map[string]time.Time)

	for {
		select {
		case <-stop:
			p.shutdown()
			return
		case <-ticker.C:
			p.tick(lastInject)
		}
	}
}

// tick 单轮采集：维护 watcher、注入命令、写入统计。
func (p *Poller) tick(lastInject map[string]time.Time) {
	for _, id := range p.reg.List() {
		inst, ok := p.reg.Get(id)
		if !ok {
			continue
		}
		if inst.Status() != "running" {
			p.dropState(id)
			p.store.Forget(id)
			continue
		}

		st := p.ensureState(id, inst)

		// 周期性注入 list/tps 以刷新基线（仅托管实例有 stdin）
		if !inst.Adopted() {
			if last, seen := lastInject[id]; !seen || time.Since(last) >= injectInterval {
				lastInject[id] = time.Now()
				go p.inject(inst, st)
			}
		}

		p.publish(id, st)
	}
}

// ensureState 确保实例有 watcher 在运行，并返回其状态。
func (p *Poller) ensureState(id string, inst *mcprocess.Instance) *instanceState {
	p.mu.Lock()
	if st, ok := p.states[id]; ok {
		p.mu.Unlock()
		return st
	}
	st := &instanceState{
		stop:    make(chan struct{}),
		players: make(map[string]struct{}),
	}
	p.states[id] = st
	p.mu.Unlock()

	// 订阅控制台输出并解析
	ch, cancel := inst.Subscribe()
	go func() {
		defer cancel()
		for {
			select {
			case <-st.stop:
				return
			case line, ok := <-ch:
				if !ok {
					return
				}
				p.parseLine(st, line)
			}
		}
	}()

	p.log.Info("开始采集实例运行时数据", "instance", id)
	return st
}

// inject 向实例 stdin 注入查询命令。
func (p *Poller) inject(inst *mcprocess.Instance, st *instanceState) {
	if err := inst.SendCommand("list"); err != nil {
		return
	}
	st.mu.Lock()
	probed, ok := st.tpsProbed, st.tpsOK
	st.mu.Unlock()
	if !probed || ok {
		_ = inst.SendCommand("tps")
	}
}

// parseLine 解析一行控制台输出。
func (p *Poller) parseLine(st *instanceState, raw string) {
	line := colorCodeRe.ReplaceAllString(raw, "")

	// list 响应
	if players, maxP, ok := parseList(line); ok {
		st.mu.Lock()
		st.maxPlayers = maxP
		// list 输出末尾是玩家名列表，用它校准集合
		if players == 0 {
			st.players = make(map[string]struct{})
		} else {
			st.players = parsePlayerNames(line)
		}
		st.mu.Unlock()
		return
	}

	// tps 响应（Paper 与 Folia 两种格式）
	if v, ok := matchFloat(tpsPaperRe, line); ok {
		st.mu.Lock()
		st.tps = v
		st.tpsProbed = true
		st.tpsOK = true
		st.mu.Unlock()
		return
	}
	if v, ok := matchFloat(tpsFoliaRe, line); ok {
		st.mu.Lock()
		st.tps = v
		st.tpsProbed = true
		st.tpsOK = true
		st.mu.Unlock()
		return
	}
	if v, ok := matchFloat(tpsFoliaLo, line); ok {
		// Folia 无 Median 行时退而取最低区域 TPS
		st.mu.Lock()
		if !st.tpsOK {
			st.tps = v
		}
		st.mu.Unlock()
		return
	}
	// 判定 tps 是否可用（未知命令）
	if strings.Contains(line, "Unknown or incomplete command") && strings.Contains(line, "tps") {
		st.mu.Lock()
		st.tpsProbed = true
		st.tpsOK = false
		st.note = "该核心不支持 tps 命令"
		st.mu.Unlock()
		return
	}

	// 玩家上下线事件
	if m := joinRe.FindStringSubmatch(line); len(m) == 2 {
		st.mu.Lock()
		st.players[m[1]] = struct{}{}
		st.mu.Unlock()
		return
	}
	if m := leftRe.FindStringSubmatch(line); len(m) == 2 {
		st.mu.Lock()
		delete(st.players, m[1])
		st.mu.Unlock()
		return
	}
}

// parseList 解析 list 命令输出，返回（在线人数, 上限, 是否匹配）。
// 形如："There are 3 of a max of 20 players online: Alice, Bob"
func parseList(line string) (players, max int32, ok bool) {
	m := listRe.FindStringSubmatch(colorCodeRe.ReplaceAllString(line, ""))
	if len(m) != 3 {
		return 0, 0, false
	}
	p, _ := strconv.Atoi(m[1])
	mp, _ := strconv.Atoi(m[2])
	return int32(p), int32(mp), true
}

// matchFloat 从正则匹配中解析浮点数。
func matchFloat(re *regexp.Regexp, line string) (float64, bool) {
	m := re.FindStringSubmatch(line)
	if len(m) != 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parsePlayerNames 从 list 输出解析玩家名集合。
// 格式："There are 2 of a max of 20 players online: Alice, Bob"
func parsePlayerNames(line string) map[string]struct{} {
	out := make(map[string]struct{})
	idx := strings.LastIndex(line, "online:")
	if idx < 0 {
		return out
	}
	tail := strings.TrimSpace(line[idx+len("online:"):])
	if tail == "" {
		return out
	}
	for _, name := range strings.Split(tail, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

// publish 将采集状态写入 Store。
func (p *Poller) publish(id string, st *instanceState) {
	st.mu.Lock()
	players := int32(len(st.players))
	maxPlayers := st.maxPlayers
	tps := st.tps
	note := st.note
	st.mu.Unlock()

	p.store.Set(id, Stats{
		TPS:        tps,
		Players:    players,
		MaxPlayers: maxPlayers,
		Updated:    time.Now(),
		Note:       note,
	})
}

// dropState 停止并移除实例的采集状态。
func (p *Poller) dropState(id string) {
	p.mu.Lock()
	st, ok := p.states[id]
	delete(p.states, id)
	p.mu.Unlock()
	if ok {
		close(st.stop)
	}
}

// shutdown 停止所有采集。
func (p *Poller) shutdown() {
	p.mu.Lock()
	states := p.states
	p.states = make(map[string]*instanceState)
	p.mu.Unlock()
	for _, st := range states {
		close(st.stop)
	}
}
