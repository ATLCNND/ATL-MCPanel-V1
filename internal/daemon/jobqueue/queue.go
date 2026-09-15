// Package jobqueue 是 Daemon 侧的节点公共任务队列。
//
// 为什么需要队列：压缩 / 解压 / 大目录复制属于「重 IO + 重 CPU」操作，
// 一次可能持续几分钟。如果直接同步执行：
//   - HTTP / gRPC 调用会长时间挂住，面板侧无法给用户任何进度反馈；
//   - 同一节点上多个实例同时发起时，磁盘 IO 会被打满，
//     直接表现为所有在线玩家的卡顿（甚至看门狗超时踢人）。
//
// 因此这里的策略是：**提交即返回，由固定数量的 worker 串行消费**。
// 节点上的重活共享同一份并发额度，谁都别想把磁盘占满。
//
// 队列是**内存态**的：Daemon 重启后任务丢失。这是有意的取舍 ——
// 持久化队列需要额外的崩溃恢复语义（半成品压缩包要不要续传？），
// 而失败任务对用户来说「重新点一次」就够了，代价远小于复杂度。
package jobqueue

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// 任务状态。
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateSuccess  = "success"
	StateFailed   = "failed"
	StateCanceled = "canceled"
)

// 任务类型。
const (
	KindCompress = "compress"
	KindExtract  = "extract"
)

// Job 一个排队任务。
type Job struct {
	ID         string `json:"job_id"`
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind"`
	Src        string `json:"src"`
	Dst        string `json:"dst"`
	Format     string `json:"format"`

	State      string `json:"state"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Error      string `json:"error"`
	TotalBytes int64  `json:"total_bytes"`
	DoneBytes  int64  `json:"done_bytes"`
	CreatedAt  int64  `json:"created_at"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`

	// 运行期字段（不对外暴露）
	cancel   context.CancelFunc `json:"-"`
	canceled bool               `json:"-"`
}

// Handler 执行一个任务。实现方应周期性调用 report 汇报进度，
// 并定期检查 ctx 是否已被取消（用户点了「取消」）。
type Handler func(ctx context.Context, job *Job, report Report) error

// Report 进度回调。
type Report func(progress int, message string, done, total int64)

// Options 队列配置。
type Options struct {
	// Workers 并发执行的 worker 数。默认 1。
	//
	// 默认取 1 而不是 CPU 核数：这类操作的瓶颈几乎总在磁盘上，
	// 并发只会让机械盘/虚拟盘陷入随机寻道，总吞吐反而下降。
	Workers int
	// Keep 已完成任务的保留时长（用于前端查看结果）。默认 30 分钟。
	Keep time.Duration
	// MaxRecords 已结束任务的保留条数上限。默认 200。
	MaxRecords int
	Logger     *slog.Logger
}

// Queue 任务队列。可安全并发使用。
type Queue struct {
	handler Handler
	workers int
	keep    time.Duration
	maxKeep int
	log     *slog.Logger

	mu      sync.Mutex
	jobs    map[string]*Job
	pending []string // 待执行的 job id，FIFO
	running int
	wake    chan struct{}
	stopped bool
}

// New 创建队列。
func New(h Handler, opts Options) *Queue {
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.Keep <= 0 {
		opts.Keep = 30 * time.Minute
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = 200
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Queue{
		handler: h,
		workers: opts.Workers,
		keep:    opts.Keep,
		maxKeep: opts.MaxRecords,
		log:     opts.Logger,
		jobs:    make(map[string]*Job),
		wake:    make(chan struct{}, 1),
	}
}

// Start 启动 worker 与清理协程。
func (q *Queue) Start() {
	for i := 0; i < q.workers; i++ {
		go q.workerLoop()
	}
	go q.reaperLoop()
}

// Submit 提交任务。job_id 为幂等键：重复提交同一个 ID 不会产生第二个任务。
// 返回排队位置（前面还有多少个任务未开始）。
func (q *Queue) Submit(j *Job) (int, bool) {
	if j == nil || j.ID == "" {
		return 0, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.stopped {
		return 0, false
	}
	if _, ok := q.jobs[j.ID]; ok {
		// 幂等：面板重试提交时直接返回既有任务
		return q.positionLocked(j.ID), true
	}

	now := time.Now().Unix()
	j.State = StateQueued
	j.Progress = 0
	j.CreatedAt = now
	j.Message = "已加入队列，等待执行"
	q.jobs[j.ID] = j
	q.pending = append(q.pending, j.ID)
	q.evictLocked()

	pos := q.positionLocked(j.ID)
	q.signal()
	return pos, true
}

// Get 查询任务状态（返回副本，调用方可自由读取）。
func (q *Queue) Get(id string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// List 返回全部任务副本（按创建时间倒序）。
func (q *Queue) List() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		out = append(out, *j)
	}
	return out
}

// Cancel 取消任务。排队中的直接标记取消；运行中的发出 context 取消信号，
// 由执行体在下一个检查点自行退出（不会强杀，以免留下半截文件）。
func (q *Queue) Cancel(id string) (string, bool) {
	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return "", false
	}
	switch j.State {
	case StateSuccess, StateFailed, StateCanceled:
		state := j.State
		q.mu.Unlock()
		return state, true
	}
	j.canceled = true
	state := j.State
	cancel := j.cancel
	if state == StateQueued {
		j.State = StateCanceled
		j.Message = "已取消（尚未开始）"
		j.FinishedAt = time.Now().Unix()
	}
	q.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return state, true
}

// Stop 停止队列（等待 worker 退出由进程退出接管）。
func (q *Queue) Stop() {
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
	q.signal()
}

// ---- 内部实现 ----

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// positionLocked 返回该任务前面还有几个未开始的任务。调用方需持锁。
func (q *Queue) positionLocked(id string) int {
	pos := 0
	for _, pid := range q.pending {
		if pid == id {
			return pos
		}
		pos++
	}
	return 0
}

func (q *Queue) workerLoop() {
	for {
		id, ctx, ok := q.next()
		if !ok {
			// 没有待执行任务：等待唤醒信号。用带超时的等待是为了
			// 在没有信号丢失的情况下也能周期性复查（防止极端时序下漏唤醒）。
			select {
			case <-q.wake:
			case <-time.After(time.Second):
			}
			if q.isStopped() {
				return
			}
			continue
		}
		q.run(ctx, id)
	}
}

// next 取出下一个待执行任务并把它置为 running。没有则返回 false。
func (q *Queue) next() (string, context.Context, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.pending) > 0 {
		id := q.pending[0]
		q.pending = q.pending[1:]
		j, ok := q.jobs[id]
		if !ok {
			continue
		}
		if j.canceled || j.State != StateQueued {
			continue // 排队期间被取消
		}
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel = cancel
		j.State = StateRunning
		j.StartedAt = time.Now().Unix()
		j.Message = "开始执行"
		q.running++
		return id, ctx, true
	}
	return "", nil, false
}

func (q *Queue) run(ctx context.Context, id string) {
	q.mu.Lock()
	cur0 := q.jobs[id]
	if cur0 == nil {
		q.running--
		q.mu.Unlock()
		return
	}
	// 用副本调用 handler：handler 在别的 goroutine 里写进度，
	// 不与查询线程共享同一份结构体
	job := *cur0
	q.mu.Unlock()

	report := func(progress int, message string, done, total int64) {
		q.mu.Lock()
		if cur, ok := q.jobs[id]; ok {
			if progress >= 0 && progress <= 100 {
				cur.Progress = progress
			}
			if message != "" {
				cur.Message = message
			}
			if total > 0 {
				cur.TotalBytes = total
			}
			if done > 0 {
				cur.DoneBytes = done
			}
		}
		q.mu.Unlock()
	}

	err := q.handler(ctx, &job, report)

	q.mu.Lock()
	q.running--
	if cur, ok := q.jobs[id]; ok {
		if cur.cancel != nil {
			cur.cancel() // 释放 ctx 资源
			cur.cancel = nil
		}
		cur.FinishedAt = time.Now().Unix()
		switch {
		case cur.canceled:
			cur.State = StateCanceled
			cur.Message = "已取消"
		case err != nil:
			cur.State = StateFailed
			cur.Error = err.Error()
			cur.Message = "执行失败"
		default:
			cur.State = StateSuccess
			cur.Progress = 100
			cur.Message = "已完成"
		}
		if err != nil {
			q.log.Warn("任务执行失败", "job", id, "kind", cur.Kind, "error", err)
		} else {
			q.log.Info("任务执行完成", "job", id, "kind", cur.Kind)
		}
	}
	q.mu.Unlock()
}

func (q *Queue) isStopped() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stopped
}

// evictLocked 清理过期与超量的已完成任务。调用方需持锁。
func (q *Queue) evictLocked() {
	if len(q.jobs) <= q.maxKeep {
		return
	}
	// 找出最旧的已结束任务逐个删除，直到回到上限内
	type entry struct {
		id string
		at int64
	}
	var finished []entry
	for id, j := range q.jobs {
		switch j.State {
		case StateSuccess, StateFailed, StateCanceled:
			finished = append(finished, entry{id, j.FinishedAt})
		}
	}
	for len(q.jobs) > q.maxKeep && len(finished) > 0 {
		oldest := 0
		for i := range finished {
			if finished[i].at < finished[oldest].at {
				oldest = i
			}
		}
		delete(q.jobs, finished[oldest].id)
		finished = append(finished[:oldest], finished[oldest+1:]...)
	}
}

func (q *Queue) reaperLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		if q.isStopped() {
			return
		}
		cutoff := time.Now().Add(-q.keep).Unix()
		q.mu.Lock()
		for id, j := range q.jobs {
			switch j.State {
			case StateSuccess, StateFailed, StateCanceled:
				if j.FinishedAt > 0 && j.FinishedAt < cutoff {
					delete(q.jobs, id)
				}
			}
		}
		q.mu.Unlock()
	}
}
