// Package dbbackup 提供面板自身数据库的备份能力。
//
// 设计要点：
//   - 使用 SQLite 的 `VACUUM INTO` 生成一致性快照。数据库运行在 WAL 模式下，
//     直接复制文件可能得到不一致/损坏的结果，因此必须用该语句（或备份 API）。
//   - 按时间戳命名，保留最近 N 份，自动清理更早的备份。
//   - Panel 启动时若距上次备份超过设定间隔则自动备份一次，此后按间隔轮询。
package dbbackup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 默认参数
const (
	DefaultKeep     = 7
	DefaultInterval = 24 * time.Hour
	filePrefix      = "panel-"
	fileSuffix      = ".db"
	timeLayout      = "20060102-150405"
)

// Manager 备份管理器。
type Manager struct {
	db       *sql.DB
	dir      string
	keep     int
	interval time.Duration

	mu       sync.Mutex
	lastRun  time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
}

// Options 构造参数。
type Options struct {
	Dir      string        // 备份目录
	Keep     int           // 保留份数（<=0 使用默认）
	Interval time.Duration // 备份间隔（<=0 使用默认）
}

// New 创建备份管理器。
func New(db *sql.DB, opts Options) *Manager {
	keep := opts.Keep
	if keep <= 0 {
		keep = DefaultKeep
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Manager{
		db:       db,
		dir:      opts.Dir,
		keep:     keep,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Backup 立即执行一次备份，返回备份文件路径。
func (m *Manager) Backup() (string, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return "", fmt.Errorf("创建备份目录失败: %w", err)
	}

	name := filePrefix + time.Now().Format(timeLayout) + fileSuffix
	dst := filepath.Join(m.dir, name)

	// 同秒内重复调用时避免文件名冲突
	if _, err := os.Stat(dst); err == nil {
		dst = filepath.Join(m.dir, fmt.Sprintf("%s%d%s",
			filePrefix, time.Now().UnixNano(), fileSuffix))
	}

	// VACUUM INTO 需要目标文件不存在；语句参数化以兼容路径中的引号
	if _, err := m.db.Exec(`VACUUM INTO ?`, dst); err != nil {
		// 兼容不支持参数绑定的驱动：退化为转义后的字面量
		escaped := strings.ReplaceAll(dst, "'", "''")
		if _, err2 := m.db.Exec(fmt.Sprintf(`VACUUM INTO '%s'`, escaped)); err2 != nil {
			return "", fmt.Errorf("备份数据库失败: %w", err)
		}
	}

	m.mu.Lock()
	m.lastRun = time.Now()
	m.mu.Unlock()

	// 清理超出保留数量的旧备份
	if _, err := m.prune(); err != nil {
		// 清理失败不影响备份本身
		return dst, nil
	}
	return dst, nil
}

// BackupInfo 备份文件信息。
type BackupInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// List 列出全部备份（按时间倒序）。
func (m *Manager) List() ([]BackupInfo, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, err
	}

	out := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), filePrefix) || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			Name:    e.Name(),
			Path:    filepath.Join(m.dir, e.Name()),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// prune 删除超出保留数量的旧备份，返回删除数量。
func (m *Manager) prune() (int, error) {
	list, err := m.List()
	if err != nil {
		return 0, err
	}
	if len(list) <= m.keep {
		return 0, nil
	}
	removed := 0
	for _, b := range list[m.keep:] {
		if err := os.Remove(b.Path); err == nil {
			removed++
		}
	}
	return removed, nil
}

// LastRun 最近一次备份时间（零值表示本次进程内尚未备份）。
func (m *Manager) LastRun() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRun
}

// Start 启动后台定时备份。首次调用时会检查是否需要立即备份。
func (m *Manager) Start() {
	// 启动即判断：距最近一份备份超过间隔则立即补一次
	if m.needBackup() {
		if _, err := m.Backup(); err != nil {
			// 交由调用方的日志记录（此处不阻断启动）
			_ = err
		}
	}

	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				if m.needBackup() {
					_, _ = m.Backup()
				}
			}
		}
	}()
}

// Stop 停止后台备份。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

// needBackup 判断是否需要执行备份（基于磁盘上最新备份的时间）。
func (m *Manager) needBackup() bool {
	list, err := m.List()
	if err != nil || len(list) == 0 {
		return true
	}
	return time.Since(list[0].ModTime) >= m.interval
}

// Dir 返回备份目录。
func (m *Manager) Dir() string { return m.dir }

// Keep 返回保留份数。
func (m *Manager) Keep() int { return m.keep }
