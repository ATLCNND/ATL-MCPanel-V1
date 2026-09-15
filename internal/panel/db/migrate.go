package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
)

// migration 一次 schema 变更。
//
// 约定：
//   - Version 从 1 开始且必须连续递增；已发布的迁移**不可修改**，只能追加新迁移
//   - 每个迁移在独立事务中执行，失败则整体回滚并终止启动
//   - 语句需保持幂等（CREATE ... IF NOT EXISTS / CREATE INDEX IF NOT EXISTS），
//     以便兼容早期由手工建表产生的数据库
type migration struct {
	Version    int
	Name       string
	Statements []string
}

// migrations 全部 schema 变更（按 Version 升序）。
var migrations = []migration{
	{
		Version: 1,
		Name:    "init_schema",
		Statements: []string{
			`CREATE TABLE IF NOT EXISTS users (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				username TEXT UNIQUE NOT NULL,
				password_hash TEXT NOT NULL,
				role TEXT NOT NULL DEFAULT 'user',
				status TEXT NOT NULL DEFAULT 'active',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE TABLE IF NOT EXISTS nodes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				ip TEXT NOT NULL,
				ssh_user TEXT NOT NULL,
				ssh_auth TEXT NOT NULL,
				ssh_port INTEGER NOT NULL DEFAULT 22,
				status TEXT NOT NULL DEFAULT 'offline',
				cpu INTEGER DEFAULT 0,
				mem INTEGER DEFAULT 0,
				last_seen DATETIME
			)`,
			`CREATE TABLE IF NOT EXISTS instances (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				instance_id TEXT UNIQUE NOT NULL,
				node_id INTEGER NOT NULL,
				name TEXT NOT NULL,
				mc_type TEXT NOT NULL DEFAULT 'folia',
				core_type TEXT NOT NULL DEFAULT 'folia',
				java_version TEXT NOT NULL DEFAULT '21',
				port INTEGER NOT NULL DEFAULT 25565,
				max_mem TEXT NOT NULL DEFAULT '2G',
				start_command TEXT DEFAULT '',
				status TEXT NOT NULL DEFAULT 'stopped',
				image_template TEXT DEFAULT '',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				FOREIGN KEY(node_id) REFERENCES nodes(id)
			)`,
			`CREATE TABLE IF NOT EXISTS instance_assignments (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				instance_id TEXT NOT NULL,
				user_id INTEGER NOT NULL,
				level TEXT NOT NULL DEFAULT 'viewer',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				UNIQUE(instance_id, user_id)
			)`,
			`CREATE TABLE IF NOT EXISTS frps_servers (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				host TEXT NOT NULL,
				bind_port INTEGER NOT NULL DEFAULT 7000,
				token TEXT NOT NULL DEFAULT '',
				port_start INTEGER NOT NULL DEFAULT 25565,
				port_end INTEGER NOT NULL DEFAULT 25600,
				remark TEXT NOT NULL DEFAULT '',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE TABLE IF NOT EXISTS tunnels (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				tunnel_id TEXT UNIQUE NOT NULL,
				instance_id TEXT NOT NULL,
				frps_id INTEGER NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				protocol TEXT NOT NULL DEFAULT 'tcp',
				local_port INTEGER NOT NULL,
				remote_port INTEGER NOT NULL,
				status TEXT NOT NULL DEFAULT 'stopped',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE TABLE IF NOT EXISTS panel_tunnel (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				enabled INTEGER NOT NULL DEFAULT 0,
				frps_id INTEGER NOT NULL DEFAULT 0,
				proxy_type TEXT NOT NULL DEFAULT 'tcp',
				remote_port INTEGER NOT NULL DEFAULT 0,
				custom_domain TEXT NOT NULL DEFAULT '',
				subdomain TEXT NOT NULL DEFAULT '',
				use_tls INTEGER NOT NULL DEFAULT 0,
				cert_file TEXT NOT NULL DEFAULT '',
				key_file TEXT NOT NULL DEFAULT '',
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE TABLE IF NOT EXISTS audit_logs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL,
				action TEXT NOT NULL,
				target TEXT DEFAULT '',
				detail TEXT DEFAULT '',
				ip TEXT DEFAULT '',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			// 索引：常见查询路径
			`CREATE INDEX IF NOT EXISTS idx_instances_node ON instances(node_id)`,
			`CREATE INDEX IF NOT EXISTS idx_assignments_user ON instance_assignments(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_assignments_instance ON instance_assignments(instance_id)`,
			`CREATE INDEX IF NOT EXISTS idx_tunnels_instance ON tunnels(instance_id)`,
			`CREATE INDEX IF NOT EXISTS idx_tunnels_frps ON tunnels(frps_id)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_logs(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name)`,
		},
	},
	{
		Version: 2,
		Name:    "nodes_add_grpc_port",
		Statements: []string{
			// 节点的 Daemon 反向 gRPC 端口（此前固定为 9091，无法在同机运行多个 Daemon）
			`ALTER TABLE nodes ADD COLUMN grpc_port INTEGER NOT NULL DEFAULT 9091`,
		},
	},
	{
		Version: 3,
		Name:    "schedules_and_alerts",
		Statements: []string{
			// 定时备份计划（每个实例一条）
			`CREATE TABLE IF NOT EXISTS backup_schedules (
				instance_id TEXT PRIMARY KEY,
				enabled INTEGER NOT NULL DEFAULT 0,
				interval_hours INTEGER NOT NULL DEFAULT 24,
				keep INTEGER NOT NULL DEFAULT 5,
				include_config INTEGER NOT NULL DEFAULT 1,
				last_run DATETIME,
				last_error TEXT NOT NULL DEFAULT '',
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			// 告警记录
			`CREATE TABLE IF NOT EXISTS alerts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				kind TEXT NOT NULL,
				severity TEXT NOT NULL DEFAULT 'warning',
				target TEXT NOT NULL,
				message TEXT NOT NULL,
				detail TEXT NOT NULL DEFAULT '',
				active INTEGER NOT NULL DEFAULT 1,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				resolved_at DATETIME
			)`,
			`CREATE INDEX IF NOT EXISTS idx_alerts_active ON alerts(active, id DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_alerts_target ON alerts(kind, target, active)`,
		},
	},
	{
		Version: 4,
		Name:    "instances_add_cpu_quota",
		Statements: []string{
			// CPU 配额百分比（100 = 1 核；0 = 不限制）。
			// 由 Daemon 通过 cgroup v2 的 cpu.max 实施，是**硬上限**。
			// 默认 0（不限制）以避免升级后意外限速生产实例。
			`ALTER TABLE instances ADD COLUMN cpu_quota INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 5,
		Name:    "nodes_add_host_stats",
		Statements: []string{
			// 节点主机资源（由 Daemon 随心跳上报），供「节点监控」页与磁盘告警使用
			`ALTER TABLE nodes ADD COLUMN cpu_percent REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE nodes ADD COLUMN mem_used INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE nodes ADD COLUMN disk_used INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE nodes ADD COLUMN disk_total INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 6,
		Name:    "backup_policies",
		Statements: []string{
			// 备份保留策略：管理员可定义多套「备份组」并指派给实例。
			// tiers 为 JSON 数组：[{"within_hours":24,"keep":4},{"within_hours":48,"keep":2}]
			// 含义：最近 24 小时保留 4 份；24~48 小时保留 2 份；更早的全部删除。
			`CREATE TABLE IF NOT EXISTS backup_policies (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL UNIQUE,
				remark TEXT NOT NULL DEFAULT '',
				auto_interval_hours INTEGER NOT NULL DEFAULT 6,
				manual_keep INTEGER NOT NULL DEFAULT 3,
				tiers TEXT NOT NULL DEFAULT '[]',
				include_config INTEGER NOT NULL DEFAULT 1,
				is_default INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			// 默认策略：自动每 6 小时一次、保存 2 天；手动保留 3 份
			`INSERT INTO backup_policies (name, remark, auto_interval_hours, manual_keep, tiers, include_config, is_default)
			 SELECT '标准（6 小时 / 保留 2 天）', '默认策略：自动每 6 小时备份，24 小时内留 4 份、48 小时内留 2 份，更早删除；手动备份保留 3 份',
			        6, 3, '[{"within_hours":24,"keep":4},{"within_hours":48,"keep":2}]', 1, 1
			 WHERE NOT EXISTS (SELECT 1 FROM backup_policies WHERE is_default = 1)`,
			// 实例的计划可指派策略（0 = 使用默认策略）
			`ALTER TABLE backup_schedules ADD COLUMN policy_id INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 13,
		Name:    "file_jobs",
		Statements: []string{
			// 排队任务：压缩 / 解压等重 IO 操作。
			//
			// 面板只做「登记 + 派发 + 回读」：真正的执行体在节点的
			// Daemon 公共队列里串行跑（避免同节点多实例互相抢磁盘）。
			// 这里落库的意义是让用户关掉页面也能回来看进度，
			// 并且给审计留痕（谁在什么时候打包了什么）。
			`CREATE TABLE IF NOT EXISTS file_jobs (
				job_id TEXT PRIMARY KEY,
				instance_id TEXT NOT NULL,
				node_id INTEGER NOT NULL,
				kind TEXT NOT NULL,
				src TEXT NOT NULL DEFAULT '',
				dst TEXT NOT NULL DEFAULT '',
				format TEXT NOT NULL DEFAULT '',
				state TEXT NOT NULL DEFAULT 'queued',
				progress INTEGER NOT NULL DEFAULT 0,
				message TEXT NOT NULL DEFAULT '',
				error TEXT NOT NULL DEFAULT '',
				total_bytes INTEGER NOT NULL DEFAULT 0,
				done_bytes INTEGER NOT NULL DEFAULT 0,
				created_by INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				started_at DATETIME,
				finished_at DATETIME
			)`,
			`CREATE INDEX IF NOT EXISTS idx_file_jobs_instance ON file_jobs(instance_id, created_at DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_file_jobs_state ON file_jobs(state)`,
		},
	},
	{
		Version: 14,
		Name:    "instance_tasks",
		Statements: []string{
			// 定时指令任务：让实例按 cron 表达式自动开机 / 关机 / 下发游戏指令。
			//
			// 与「定时备份」分开建表的原因：备份是单一动作（一条/实例），
			// 而这里一个实例可以有多条任务、动作类型也不同，且**普通用户
			// （实例 owner）就能创建**，需要独立的权限与审计口径。
			`CREATE TABLE IF NOT EXISTS instance_tasks (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				instance_id TEXT NOT NULL,
				name TEXT NOT NULL DEFAULT '',
				action TEXT NOT NULL,
				command TEXT NOT NULL DEFAULT '',
				cron TEXT NOT NULL,
				enabled INTEGER NOT NULL DEFAULT 1,
				created_by INTEGER NOT NULL DEFAULT 0,
				last_run DATETIME,
				last_state TEXT NOT NULL DEFAULT '',
				last_error TEXT NOT NULL DEFAULT '',
				run_count INTEGER NOT NULL DEFAULT 0,
				fail_count INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_instance_tasks_instance ON instance_tasks(instance_id, id)`,
			`CREATE INDEX IF NOT EXISTS idx_instance_tasks_enabled ON instance_tasks(enabled)`,
		},
	},
	{
		Version: 15,
		Name:    "node_admins",
		Statements: []string{
			// 节点管理员：可以管理**被指定节点**上的实例（创建 / 删除 / 到期时间），
			// 其余权限与普通用户相同。
			//
			// 为什么用独立关联表而不是往 users.role 里塞一个逗号分隔的节点列表：
			//   - 一个用户可能同时管多个节点，之后还可能管三个五个；
			//   - 授权与回收是成对操作，关联表删一行就干净回收，
			//     而字符串列表要读-改-写，并发下容易丢更新；
			//   - 需要按节点反查"谁能管这台"，关联表加个索引就够。
			`CREATE TABLE IF NOT EXISTS node_admins (
				user_id INTEGER NOT NULL,
				node_id INTEGER NOT NULL,
				granted_by INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (user_id, node_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_node_admins_node ON node_admins(node_id)`,
			`CREATE INDEX IF NOT EXISTS idx_node_admins_user ON node_admins(user_id)`,
		},
	},
	{
		Version: 16,
		Name:    "instances_add_expiry",
		Statements: []string{
			// 实例到期时间（NULL = 永不到期）。
			//
			// 语义由 scheduler 实现：临近到期提前告警，到期后**自动停止**实例
			// 但**不删数据** —— 与磁盘超限自动停机同一套取舍：
			// 停机有破坏性（踢人、可能丢未落盘进度），但比删数据温和得多，
			// 而且管理员随时可以延长到期时间或手动重启。
			`ALTER TABLE instances ADD COLUMN expires_at DATETIME`,
			// 到期提醒的提前天数（0 = 用全局默认）
			`ALTER TABLE instances ADD COLUMN expiry_notice_days INTEGER NOT NULL DEFAULT 3`,
			// 到期是否自动停止（默认开 —— 用户选了"到期自动停止"的方案）
			`ALTER TABLE instances ADD COLUMN expiry_autostop INTEGER NOT NULL DEFAULT 1`,
			`CREATE INDEX IF NOT EXISTS idx_instances_expires ON instances(expires_at)`,
		},
	},
	{
		Version: 17,
		Name:    "node_users_and_port_quota",
		Statements: []string{
			// 角色改名：nodeadmin → nodeuser（「节点管理员」→「节点用户」）。
			//
			// 这个角色的定位变了：不再是"节点的二把手"（那种定位会让人以为
			// 他能管节点上的一切），而是"被分配了穿透端口的普通用户"。
			// 名字必须跟着定位走，否则权限边界永远说不清。
			`UPDATE users SET role = 'nodeuser' WHERE role = 'nodeadmin'`,

			// 表名同步：node_admins → node_users
			`ALTER TABLE node_admins RENAME TO node_users`,

			// 穿透端口配额：管理员预先给节点用户在**每条线路**上分配可用端口数。
			//
			// 按 (user, frps) 记录而不是给一个总数：端口是按线路分配的
			//（不同 frps 服务的端口段、带宽、地域都可能不同），
			// 只给总数就没法回答"我能在哪条线路上开几个"。
			`CREATE TABLE IF NOT EXISTS node_user_ports (
				user_id INTEGER NOT NULL,
				frps_id INTEGER NOT NULL,
				quota INTEGER NOT NULL DEFAULT 0,
				granted_by INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (user_id, frps_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_node_user_ports_user ON node_user_ports(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_node_user_ports_frps ON node_user_ports(frps_id)`,

			// 实例记录创建者：配额用量由「该用户创建的实例占用的隧道数」推导，
			// 因此需要知道实例是谁建的。管理员代建时也记管理员，不影响统计口径
			//（管理员本身不受配额限制）。
			`ALTER TABLE instances ADD COLUMN created_by INTEGER NOT NULL DEFAULT 0`,
			`CREATE INDEX IF NOT EXISTS idx_instances_created_by ON instances(created_by)`,
		},
	},
	{
		Version: 12,
		Name:    "instances_add_disk_limit",
		Statements: []string{
			// 实例目录的软配额（MB，0 = 不限制）。
			//
			// 说明：这是**事后巡检**而非内核实时限制 —— ext4 没有目录级配额，
			// XFS 项目配额又要求实例目录位于 XFS 上。当前实现由面板定期检查
			// 用量并分级告警，可选超限自动停机，防止单个实例写满整盘。
			`ALTER TABLE instances ADD COLUMN disk_limit_mb INTEGER NOT NULL DEFAULT 0`,
			// 超过硬上限时是否自动停止实例（默认关闭：停机是有破坏性的操作，
			// 不应在用户未明确选择时自动执行）
			`ALTER TABLE instances ADD COLUMN disk_autostop INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 11,
		Name:    "instance_metrics_history",
		Statements: []string{
			// 实例指标历史采样。
			//
			// 此前只有实时指标（/metrics 直接问 Daemon），刷新页面即丢失，
			// 导致「统计」页无从展示趋势、也无法回溯"昨晚为什么卡"。
			// 由面板调度器定期采样，数据量很小（每实例每分钟一行）。
			`CREATE TABLE IF NOT EXISTS instance_metrics (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				instance_id TEXT NOT NULL,
				cpu_percent REAL NOT NULL DEFAULT 0,
				mem_used INTEGER NOT NULL DEFAULT 0,
				players INTEGER NOT NULL DEFAULT 0,
				tps REAL NOT NULL DEFAULT 0,
				sampled_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_metrics_instance_time ON instance_metrics(instance_id, sampled_at DESC)`,
		},
	},
	{
		Version: 10,
		Name:    "instances_add_mem_limit",
		Statements: []string{
			// cgroup memory.max（如 "4G"；空 = 不限制）。由 Daemon 施加。
			// 默认留空：已存在的实例不应因为升级而被加上限制导致 OOM。
			`ALTER TABLE instances ADD COLUMN mem_limit TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 9,
		Name:    "users_profile_and_avatar",
		Statements: []string{
			// 头像：文件名由服务端按用户 ID 生成，状态用于审核流
			`ALTER TABLE users ADD COLUMN avatar_file TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE users ADD COLUMN avatar_status TEXT NOT NULL DEFAULT 'none'`,
			`ALTER TABLE users ADD COLUMN avatar_note TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE users ADD COLUMN avatar_reviewed_by INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE users ADD COLUMN avatar_reviewed_at DATETIME`,
			// 累计在线时长（秒）。以相邻请求间隔累加，单次上限 5 分钟。
			`ALTER TABLE users ADD COLUMN total_online_seconds INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE users ADD COLUMN last_active_at DATETIME`,
			`CREATE INDEX IF NOT EXISTS idx_users_avatar_status ON users(avatar_status)`,
		},
	},
	{
		Version: 8,
		Name:    "tunnels_add_display_domain",
		Statements: []string{
			// 管理员为该隧道配置的对外域名（可含端口），用于实例页展示。
			// 为空时前端回退显示 IP:端口。
			`ALTER TABLE tunnels ADD COLUMN display_domain TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 7,
		Name:    "nodes_add_backup_disk",
		Statements: []string{
			// 备份所在分区的水位。
			// 冷存储常位于独立的机械盘，与实例盘不同，需要单独监控与告警。
			`ALTER TABLE nodes ADD COLUMN backup_disk_used INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE nodes ADD COLUMN backup_disk_total INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 18,
		Name:    "announcements_and_help",
		Statements: []string{
			// 公告：管理员发布，所有登录用户可见。
			//
			// 两个布尔位分开：
			//   pinned    —— 置顶（如"维护通知"要一直挂在最上面）
			//   published —— 下架。**用下架而不是删除**：公告是"当时对用户说过什么"
			//                的记录，删掉之后历史就无从追溯了。
			`CREATE TABLE IF NOT EXISTS announcements (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				title TEXT NOT NULL,
				body TEXT NOT NULL DEFAULT '',
				pinned INTEGER NOT NULL DEFAULT 0,
				published INTEGER NOT NULL DEFAULT 1,
				created_by INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			// 列表查询固定按"是否发布 + 是否置顶 + 时间倒序"，直接建复合索引
			`CREATE INDEX IF NOT EXISTS idx_announcements_visible
				ON announcements(published, pinned, created_at)`,

			// 帮助文档：永远只有一份，所以用 id 固定为 1 的**单行表**。
			//
			// 为什么不放文件（如 data/docs/help.md）：
			//   1. 要在界面上直接编辑，单行表自带 updated_at / updated_by，
			//      能回答"谁在什么时候改的"；放文件还得自己管并发写与权限
			//   2. 备份数据库时它跟着一起走，不用再操心第二个数据源
			`CREATE TABLE IF NOT EXISTS help_doc (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				content TEXT NOT NULL DEFAULT '',
				updated_by INTEGER NOT NULL DEFAULT 0,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			// 先塞一行空文档，省得后面每条查询都要处理"还没有行"的情况
			`INSERT OR IGNORE INTO help_doc (id, content) VALUES (1, '')`,
		},
	},
	{
		Version: 19,
		Name:    "frps_display_domain",
		Statements: []string{
			// 线路（frps 服务器）级的**对外域名**。
			//
			// 为什么需要它（原来只有隧道级 display_domain）：
			//   1. 一条线路上有很多实例、很多端口，逐个隧道填域名既繁琐又必然漏
			//   2. 更要紧的是**隐私**：没有域名时，实例页只能给用户显示
			//      `节点IP:端口` —— 用户一转发就把节点的真实入口地址公布了。
			//      线路级域名能让所有端口统一显示成 `mc.example.com:端口`。
			//
			// 存**纯主机名**（不带端口）：端口每个隧道都不同，拼装时才追加。
			// 管理员若填了 `https://x.com/` 或 `x.com:25570`，写入时会归一化。
			`ALTER TABLE frps_servers ADD COLUMN display_domain TEXT NOT NULL DEFAULT ''`,
		},
	},
}

// migrate 应用尚未执行的迁移。
func migrate(d *sql.DB) error {
	if err := ensureMigrationsTable(d); err != nil {
		return err
	}

	// 按版本号排序后再执行。
	// 迁移列表是按时间追加的，难免出现"新版本插在旧版本之前"的情况
	// （曾出现过实际执行顺序为 …6,10,9,8,7）。虽然每个迁移彼此独立时
	// 不影响结果，但顺序一旦被依赖就会静默出错，因此这里强制排序。
	ordered := make([]migration, len(migrations))
	copy(ordered, migrations)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })

	current, err := CurrentVersion(d)
	if err != nil {
		return err
	}

	pending := 0
	for _, m := range ordered {
		if m.Version <= current {
			continue
		}
		if err := applyMigration(d, m); err != nil {
			return fmt.Errorf("迁移 %d(%s) 失败: %w", m.Version, m.Name, err)
		}
		slog.Info("数据库迁移已应用", "version", m.Version, "name", m.Name)
		pending++
	}
	if pending == 0 {
		slog.Info("数据库 schema 已是最新", "version", current)
	}
	return nil
}

// ensureMigrationsTable 创建迁移版本表。
func ensureMigrationsTable(d *sql.DB) error {
	_, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

// CurrentVersion 返回当前已应用的最高版本（无记录返回 0）。
func CurrentVersion(d *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// applyMigration 在事务中执行单个迁移并记录版本。
func applyMigration(d *sql.DB, m migration) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for i, stmt := range m.Statements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("第 %d 条语句出错: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.Version, m.Name); err != nil {
		return err
	}
	return tx.Commit()
}

// MigrationInfo 迁移的只读描述（供测试与运维查看）。
type MigrationInfo struct {
	Version int
	Name    string
}

// AllMigrations 返回全部迁移描述（升序）。
func AllMigrations() []MigrationInfo {
	out := make([]MigrationInfo, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, MigrationInfo{Version: m.Version, Name: m.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// LatestVersion 返回代码中包含的最高迁移版本。
func LatestVersion() int {
	v := 0
	for _, m := range migrations {
		if m.Version > v {
			v = m.Version
		}
	}
	return v
}
