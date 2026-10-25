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
	{
		Version: 20,
		Name:    "logshare_uploads",
		Statements: []string{
			// LogShare.CN 日志分析的上传记录。
			//
			// 为什么必须落库（而不是"传完就算"）：
			//   1. **删除需要 token**：对方 API 的删除接口要上传时返回的 token，
			//      丢了就再也删不掉那份日志（官方文档明确要求调用方自行持久化）。
			//      日志里含玩家名与聊天内容，删不掉是隐私问题，不是小疏忽。
			//   2. **要能回答"谁把哪台实例的日志传出去了"**：审计需要。
			//   3. 到期时间（默认 15 天）要能提前提醒，也让用户知道何时会自动消失。
			`CREATE TABLE IF NOT EXISTS logshare_uploads (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				instance_id TEXT NOT NULL,
				logshare_id TEXT NOT NULL,
				token TEXT NOT NULL DEFAULT '',
				url TEXT NOT NULL DEFAULT '',
				source_path TEXT NOT NULL DEFAULT '',
				size INTEGER NOT NULL DEFAULT 0,
				lines INTEGER NOT NULL DEFAULT 0,
				filtered_lines INTEGER NOT NULL DEFAULT 0,
				truncated INTEGER NOT NULL DEFAULT 0,
				uploaded_by INTEGER NOT NULL DEFAULT 0,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				expires_at DATETIME,
				deleted_at DATETIME
			)`,
			`CREATE INDEX IF NOT EXISTS idx_logshare_instance ON logshare_uploads(instance_id, id DESC)`,
			// AI 分析结论（Markdown）留一份：同一份日志没必要反复消耗对方的 AI 资源，
			// 也方便用户回头看"上次那个报错是怎么说的"。
			`CREATE TABLE IF NOT EXISTS logshare_analyses (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				instance_id TEXT NOT NULL,
				logshare_id TEXT NOT NULL,
				content TEXT NOT NULL DEFAULT '',
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				UNIQUE(instance_id, logshare_id)
			)`,
		},
	},
	{
		Version: 21,
		Name:    "panel_settings",
		Statements: []string{
			// 面板级运行时开关（键值对）。
			//
			// 为什么需要它，而不是改 config.yaml：像"第三方日志分析开关"这种
			// **管理员在界面上随手一按就该生效**的项，让他去改配置文件再重启服务，
			// 结果就是"这个开关没人用、功能只好一直开着" —— 而它涉及把日志发给第三方，
			// 默认状态与切换成本都很敏感。
			//
			// 取值优先级：这里的记录 > config.yaml 的默认值。
			// 这样"老配置仍然生效"（没记录时按配置走），而界面上改过之后以界面为准。
			`CREATE TABLE IF NOT EXISTS panel_settings (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL DEFAULT '',
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
		},
	},
	{
		Version: 22,
		Name:    "analysis_providers",
		Statements: []string{
			// 日志分析的"提供方"：把原来写死的 LogShare 变成可插拔的一层。
			//
			// 为什么需要这张表（2026-09-30 定的 D1）：
			//   1. LogShare 是公益合作的首选，但它会维护、会限流、会改接口 ——
			//      把"分析"绑死在它一家上，对方一停我们就整条链路不可用；
			//   2. 用户/管理员应该能接**自己的**平台（免费额度、自建网关、商业 API），
			//      用自己的额度、自己的隐私边界；
			//   3. mclo.gs 这类"不是 AI、但能把日志变成可分享链接"的服务也要能挂在同一条链上。
			//
			// owner_id：0 = 全局（总管理员建的，所有人可用）；>0 = 某个用户的私有配置。
			// D1 的决定是"管理员 + 节点用户可以各配自己的"，普通用户只能用全局的。
			`CREATE TABLE IF NOT EXISTS analysis_providers (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				kind TEXT NOT NULL DEFAULT 'openai',      -- logshare / mclogs / openai / builtin-rules
				base_url TEXT NOT NULL DEFAULT '',
				model TEXT NOT NULL DEFAULT '',
				api_key_enc BLOB,                          -- AES-GCM 密文；明文永不落库、永不回显
				key_hint TEXT NOT NULL DEFAULT '',          -- 仅末 4 位，供界面显示 ****abcd
				max_bytes INTEGER NOT NULL DEFAULT 0,
				timeout_sec INTEGER NOT NULL DEFAULT 300,
				prompt TEXT NOT NULL DEFAULT '',
				owner_id INTEGER NOT NULL DEFAULT 0,
				enabled INTEGER NOT NULL DEFAULT 1,
				created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_analysis_providers_owner ON analysis_providers(owner_id, enabled)`,

			// 上传记录表补三列：现在这条链上不止 LogShare 一家了。
			//   provider_id / provider_kind —— 这份记录是哪家产生的（历史列表要显示、删除要路由对家）；
			//   raw_url —— mclo.gs 的原文直链（求助文本里要用）；
			//   errors —— mclo.gs 会数 ERROR 行，这是它唯一的"诊断信号"。
			`ALTER TABLE logshare_uploads ADD COLUMN provider_id INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE logshare_uploads ADD COLUMN provider_kind TEXT NOT NULL DEFAULT 'logshare'`,
			`ALTER TABLE logshare_uploads ADD COLUMN raw_url TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE logshare_uploads ADD COLUMN errors INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 23,
		Name:    "users_token_version",
		Statements: []string{
			// 令牌版本号：JWT 里带上它，请求时与库里的值比对，不一致即失效。
			//
			// 为什么需要（2026-10-01 安全审查）：JWT 是**自证明**的，签发之后
			// 面板无从撤销。原先令牌有效期 24 小时，于是"管理员把被盗账号删掉 /
			// 重置密码"这个动作在最长 24 小时内**完全不生效** —— 攻击者继续用
			// 旧令牌，甚至继续以 admin 身份操作。
			//
			// 改密码、被删号时把这个值 +1（删号那条同时靠"用户查不到"失效），
			// 旧令牌立刻作废。
			`ALTER TABLE users ADD COLUMN token_version INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 24,
		Name:    "frp_line_types",
		Statements: []string{
			// 线路类型（底账 F16）。三种出网方式，用一个字段区分：
			//
			//   self_hosted  自建 frps —— 现有的那套：有端口段，面板从段里分配 remote_port
			//   third_party  第三方单隧道 —— 服务商给一条固定隧道（远端端口或子域），
			//                没有端口段，值来自用户导入的配置
			//   direct       公网直出 —— 节点自己有公网 IP，**不走任何穿透**：
			//                地址就是「线路主机:本地端口」，面板不跑 frpc
			//
			// 为什么放在 frps_servers 上而不是新开一张表：这三种在面板里都是「一条出网线路」，
			// 隧道（tunnels.frps_id）已经指向它了。为新类型另开一张表，等于让
			// "按线路查隧道"这件事裂成两套查询。
			`ALTER TABLE frps_servers ADD COLUMN line_type TEXT NOT NULL DEFAULT 'self_hosted'`,

			// 第三方隧道的域名方式（vhost）与 TLS 开关。
			//
			// 照 panel_tunnel 表里那几列搬过来（那边早就有了），**不再另造一套命名**：
			// 同一个概念在两处用两个名字，是这类项目最常见的返工来源。
			`ALTER TABLE tunnels ADD COLUMN subdomain TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE tunnels ADD COLUMN custom_domain TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE tunnels ADD COLUMN use_tls INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 25,
		Name:    "frp_auth_mode",
		Statements: []string{
			// 第三方服务商的 token 放在哪个字段（底账 F16）。
			//
			//	auth      —— auth.method + auth.token（frp 官方写法，SakuraFRP 这类）
			//	metadatas —— metadatas.token（Lolia 这类自建 frps 的服务商读它）
			//
			// 实测依据（内测节点，bundled frpc 0.61.1 对 Lolia）：
			//   只发 auth.token            → 服务端回「FRPC 配置文件错误」，登录即失败
			//   user + metadatas.token     → login to server success
			//   user + 两者都发             → token in login doesn't match（多余的那个反而坏了）
			// 所以必须精确按服务商要求只发一种，不能"都发上更保险"。
			`ALTER TABLE frps_servers ADD COLUMN auth_mode TEXT NOT NULL DEFAULT 'auth'`,

			// user：frp 的正式字段，服务商用它区分租户。Lolia 不写就登不上。
			`ALTER TABLE frps_servers ADD COLUMN user TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 26,
		Name:    "tunnel_transport",
		Statements: []string{
			// 服务商分配的传输参数（底账 F16）。
			//
			// 实测：Lolia 的服务端把 bandwidthLimit 当成**隧道定义的一部分**校验 ——
			// 同一个 name + remotePort，缺了 transport 段一律回「FRPC 配置文件错误」，
			// 带上就 start proxy success。所以导入时必须把这两项存下来并原样下发。
			`ALTER TABLE tunnels ADD COLUMN bandwidth_limit TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE tunnels ADD COLUMN bandwidth_limit_mode TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 27,
		Name:    "node_public_ip",
		Statements: []string{
			// 节点的**公网地址**（底账 F16 公网直出）。
			//
			// 为什么必须与 ip 分开：ip 是"面板/运维怎么连这台机器"（常常是内网或
			// 127.0.0.1），而公网直出要给**玩家**一个地址 —— 两者经常不是同一个值。
			// 之前没有这一列，于是"这条直连线路是不是这个节点的"根本无从判断，
			// 自检也只能拿内网 IP 去比对，提示是错的。
			`ALTER TABLE nodes ADD COLUMN public_ip TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 28,
		Name:    "tunnel_transport_tls",
		Statements: []string{
			// 传输层 TLS 的两个开关（底账 F16，第三方服务商常有）。
			//
			// 存**三态文本**而不是布尔：空串 = 没写、沿用 frp 默认；
			// 'true'/'false' = 服务商显式要求。SakuraFRP 要求显式 false，
			// 而 frp 默认是 true —— 分不出这两者就会「TCP 连上但服务端不响应」。
			`ALTER TABLE tunnels ADD COLUMN tls_enable TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE tunnels ADD COLUMN tls_disable_custom_first_byte TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 29,
		Name:    "node_auth_type",
		Statements: []string{
			// 节点凭据的**类型**：'' = 未指定（老数据，按内容自动判断）/ 'password' / 'key'。
			//
			// 为什么要显式存：以前靠「字符串里有没有 PRIVATE KEY」去猜 —— 猜错的后果
			// 很隐蔽（私钥少一行就被当成密码，最后只报一句认证失败，用户不知道错在哪）。
			// 面板也要把这个值原样展示出来（列表里能看见「密钥 / 密码」）。
			//
			// 空串必须保留：升级前登记的节点没有这个字段，行为要与以前完全一致。
			`ALTER TABLE nodes ADD COLUMN ssh_auth_type TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 30,
		Name:    "node_panel_port",
		Statements: []string{
			// 该节点连面板用的 gRPC 端口（0 = 用面板自己的配置）。
			//
			// 为什么需要可覆盖：面板的 9090 可能被 NAT/端口映射改过、同机有多个面板、
			// 或者一个面板要给不同网络环境的节点用不同入口 —— 写死一个端口时，
			// 用户只能去改全局配置（连带影响所有节点）。
			`ALTER TABLE nodes ADD COLUMN panel_grpc_port INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 31,
		Name:    "node_enroll_mode",
		Statements: []string{
			// 这个节点是**怎么接进来的**（决定界面上哪些操作可用）：
			//
			//   ''            未标注（升级前的存量节点）
			//   ssh           一键部署远端节点（面板用 SSH 推，需要凭据）
			//   panel         凭证交换 · 面板发起（M4）
			//   node          凭证交换 · 节点发起（M3）
			//   local-probe   本机探测接入（同机已有 Daemon）
			//   local-install 本机一键部署（面板自己装 Daemon）
			//
			// 有了它界面才能实事求是：本机节点不该提示「配置凭据」，凭证交换接入的节点
			// 不该提供「重新推二进制」。
			`ALTER TABLE nodes ADD COLUMN enroll_mode TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 32,
		Name:    "node_uid",
		Statements: []string{
			// 节点的**稳定身份**（Daemon 首次启动生成、随注册上报）。
			//
			// 与 name 的分工：name 是显示名（可改、不同面板里可不同）；node_uid 跟着这台机器
			// 的安装走。凭证交换用它做唯一身份，多面板（V2）也以它为准。
			//
			// 空串 = 升级前的老节点，或非常老的 Daemon（不上报该字段）。唯一索引要排除空串，
			// 否则所有老节点会互相冲突。
			`ALTER TABLE nodes ADD COLUMN node_uid TEXT NOT NULL DEFAULT ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_node_uid ON nodes(node_uid) WHERE node_uid <> ''`,
		},
	},
	{
		Version: 33,
		Name:    "credential_exchange",
		Statements: []string{
			// 一次性 join token（凭证交换）。
			//
			// mode: panel = 面板发起（token 由面板签、管理员贴到节点上执行）
			//       node  = 节点发起（Daemon 生成并自签名，管理员把 token 粘回面板）
			// 语义见 docs/DESIGN-CREDENTIAL-EXCHANGE.md §4。
			//
			// jti 唯一：消耗时用 `UPDATE ... WHERE used_at IS NULL` 原子抢占，天然防并发重放。
			`CREATE TABLE IF NOT EXISTS join_tokens (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				jti         TEXT NOT NULL UNIQUE,
				mode        TEXT NOT NULL,
				node_uid    TEXT NOT NULL DEFAULT '',
				node_name   TEXT NOT NULL DEFAULT '',
				created_by  INTEGER NOT NULL DEFAULT 0,
				created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				expires_at  DATETIME NOT NULL,
				used_at     DATETIME,
				used_from   TEXT NOT NULL DEFAULT '',
				revoked_at  DATETIME
			)`,

			// 每次签发/续期的记录。撤销查这张表；续期要求公钥指纹一致。
			`CREATE TABLE IF NOT EXISTS node_enrollments (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				node_uid    TEXT NOT NULL,
				cert_fp     TEXT NOT NULL DEFAULT '',
				pubkey_fp   TEXT NOT NULL DEFAULT '',
				issued_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				expires_at  DATETIME NOT NULL,
				revoked_at  DATETIME,
				reason      TEXT NOT NULL DEFAULT '',
				replaced_by TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX IF NOT EXISTS idx_enrollments_uid ON node_enrollments(node_uid)`,
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
