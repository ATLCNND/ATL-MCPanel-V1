package httpapi

import (
	"context"
	"net/http"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ============================================================================
// 世界信息与游戏规则（GET /api/instances/{id}/world）
// ============================================================================
//
// 背景（2026-10-05 用户要求）：玩家页与文件页之间需要一个「游戏设置」页，
// 展示世界基本信息并管理原版规则。
//
// 数据来源是**节点的 level.dat**（NBT），由 Daemon 解析后返回结构化数据
//（见 daemon/grpcapi/worldinfo.go 里的说明：为什么不在面板侧解析二进制）。
//
// 为什么只读、规则改动走控制台：
//   - 读取：直接解析 level.dat 的 GameRules 就能拿到全部 51 条当前值，
//     比"发 /gamerule 再解析回显"可靠（后者要求服务器在运行）；
//   - 修改：**必须**走 `/gamerule` 指令（见 handleSetGameRule）。运行中的服务器
//     会把 level.dat 里的 GameRules 覆写掉，直接改文件属于典型的"改了却没变"。
//
// 权限：读需要能看见该实例（viewer 及以上）；改规则需要能发控制台指令
//（owner/collab 及以上，与 ConsoleTab 的 canSend 同一判据）。

// worldInfoView 给前端的结构。字段名与 proto 保持一致（省得两边对不上），
// 但把几个"原始值"换成界面上直接可用的形态。
type worldInfoView struct {
	OK         bool   `json:"ok"`
	WorldDir   string `json:"world_dir"`
	LevelFound bool   `json:"level_dat_found"`
	LevelName  string `json:"level_name"`

	// 种子：字符串返回（可能是负数、可能超出 JS 安全整数范围 —— 实测
	// 1283372119871044237 > 2^53，用 number 传会在前端被改掉末几位，
	// 所以**必须**用字符串）。
	Seed        string `json:"seed"`
	SeedPresent bool   `json:"seed_present"`
	Generator   string `json:"generator"`
	// 生成结构 / 奖励箱：Java 版里没有"世界类型"这个独立字段，
	// 由 generator.type + 这些开关共同表达，界面按这个组合来描述。
	GenerateFeatures bool `json:"generate_features"`
	BonusChest       bool `json:"bonus_chest"`

	GameType         int32 `json:"game_type"`
	Difficulty       int32 `json:"difficulty"`
	DifficultyLocked bool  `json:"difficulty_locked"`
	Hardcore         bool  `json:"hardcore"`
	AllowCommands    bool  `json:"allow_commands"`

	SpawnX int32 `json:"spawn_x"`
	SpawnY int32 `json:"spawn_y"`
	SpawnZ int32 `json:"spawn_z"`

	// 世界边界。6e7 是"未设置"的哨兵值，这里**已经换算好**：界面上直接看
	// border_set 就知道该显示数值还是"未设置"，不必让前端也记这个魔法数。
	BorderSize    float64 `json:"border_size"`
	BorderSet     bool    `json:"border_set"`
	BorderCenterX float64 `json:"border_center_x"`
	BorderCenterZ float64 `json:"border_center_z"`

	Time         int64 `json:"time"`
	DayTime      int64 `json:"day_time"`
	Raining      bool  `json:"raining"`
	Thundering   bool  `json:"thundering"`
	LastPlayedMs int64 `json:"last_played_ms"`

	DataVersion  int32    `json:"data_version"`
	ServerBrands []string `json:"server_brands"`

	Gamerules         []gameruleView `json:"gamerules"`
	DatapacksEnabled  []string       `json:"datapacks_enabled"`
	DatapacksDisabled []string       `json:"datapacks_disabled"`
	Dimensions        []string       `json:"dimensions"`
}

type gameruleView struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// borderDisabledSentinel 世界边界"未设置"的哨兵值。
//
// 原版在边界未设置时把 BorderSize 写成 6e7（6000 万）。直接显示会让用户
// 以为"边界设成了 6000 万格"，所以在服务端就换算成 border_set=false。
//
// ⚠️ 容差必须够大：实测某个真实存档里这个值存成了 **59999968**（不是精确的
// 60000000）—— 浮点经由 double 存储/序列化会有偏差。一开始用 ±1 比较，
// 于是它被判成"已设置"，界面显示"59,999,968 格"，纯属胡说。
// 真实的边界值上限是 ±29999984（原版硬限制），所以只要**远大于** 3e7
// 就一定是哨兵。
const borderDisabledSentinel = 6.0e7

// borderIsSet 判断边界是不是真的被设置过。
//
// 判据：0（从未设置）与"接近 6e7 的哨兵"都算未设置；其余算已设置。
// 用 1e6 的容差：真实边界不可能接近 6e7，而哨兵值即使有浮点偏差也在百万分之一内。
func borderIsSet(size float64) bool {
	if size == 0 {
		return false
	}
	return !(size > borderDisabledSentinel-1e6 && size < borderDisabledSentinel+1e6)
}

// handleGetWorldInfo GET /api/instances/{id}/world
func (s *Server) handleGetWorldInfo(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")

	// 读权限：能看见这台实例即可（viewer 也允许看世界信息）
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "节点不可达："+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	world := r.URL.Query().Get("world")
	resp, err := cli.ReadWorldInfo(ctx, &pb.WorldInfoRequest{
		InstanceId:       instanceID,
		World:            world,
		IncludeGamerules: true,
	})
	if err != nil {
		// 把 gRPC 的原始错误带上：这条链路上有 mTLS、节点可达性、RPC 版本差异
		// 好几个可能出问题的地方，"读取失败"这种笼统说法在排查时等于没说。
		writeErr(w, http.StatusBadGateway, "读取世界信息失败（调用节点出错）："+err.Error())
		return
	}
	if resp == nil {
		// 正常情况下不会走到这里；真走到了说明节点侧返回了空响应，
		// 那种情况必须留下痕迹，而不是当成"没数据"。
		writeErr(w, http.StatusBadGateway, "节点返回了空响应（Daemon 版本可能过旧，未实现该项）")
		return
	}
	if !resp.Success {
		msg := resp.Error
		if msg == "" {
			// 节点说失败但没给原因 —— 这本身是个 bug（daemon 侧应当总是带 Error），
			// 如实报出来而不是编一个含糊的话，否则排查时会被误导。
			msg = "节点拒绝了这次读取，但没有给出原因（Daemon 侧应当带上 Error 字段）"
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	view := worldInfoView{
		OK:               true,
		WorldDir:         resp.WorldDir,
		LevelFound:       resp.LevelDatFound,
		LevelName:        resp.LevelName,
		Seed:             resp.Seed,
		SeedPresent:      resp.SeedPresent,
		Generator:        resp.Generator,
		GenerateFeatures: resp.GenerateFeatures,
		BonusChest:       resp.BonusChest,
		GameType:         resp.GameType,
		Difficulty:       resp.Difficulty,
		DifficultyLocked: resp.DifficultyLocked,
		Hardcore:         resp.Hardcore,
		AllowCommands:    resp.AllowCommands,
		SpawnX:           resp.SpawnX,
		SpawnY:           resp.SpawnY,
		SpawnZ:           resp.SpawnZ,
		BorderSize:       resp.BorderSize,
		// 哨兵值 → "未设置"（判据见 borderIsSet 的说明：实测值可能是
		// 59999968 而不是精确的 6e7，容差不能只有 1）
		BorderSet:         borderIsSet(resp.BorderSize),
		BorderCenterX:     resp.BorderCenterX,
		BorderCenterZ:     resp.BorderCenterZ,
		Time:              resp.Time,
		DayTime:           resp.DayTime,
		Raining:           resp.Raining,
		Thundering:        resp.Thundering,
		LastPlayedMs:      resp.LastPlayedMs,
		DataVersion:       resp.DataVersion,
		ServerBrands:      resp.ServerBrands,
		DatapacksEnabled:  resp.DatapacksEnabled,
		DatapacksDisabled: resp.DatapacksDisabled,
		Dimensions:        resp.Dimensions,
	}
	for _, g := range resp.Gamerules {
		view.Gamerules = append(view.Gamerules, gameruleView{Name: g.Name, Value: g.Value})
	}
	// 空切片而不是 null：前端 .map / .length 时不用再判空。
	// ⚠️ 每个数组字段都要在这里归一 —— 漏一个，前端就会收到 null，
	// 而 `null.length` 会直接抛异常（实测 datapacks_* 漏过一次）。
	if view.ServerBrands == nil {
		view.ServerBrands = []string{}
	}
	if view.Gamerules == nil {
		view.Gamerules = []gameruleView{}
	}
	if view.Dimensions == nil {
		view.Dimensions = []string{}
	}
	if view.DatapacksEnabled == nil {
		view.DatapacksEnabled = []string{}
	}
	if view.DatapacksDisabled == nil {
		view.DatapacksDisabled = []string{}
	}

	writeJSON(w, http.StatusOK, view)
}
