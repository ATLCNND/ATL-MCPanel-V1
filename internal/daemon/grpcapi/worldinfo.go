package grpcapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/nbt"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ReadWorldInfo 解析实例的 level.dat，返回一份**只读**的世界快照。
//
// 为什么不复用 ReadFile：那条是文本接口（Content 是 string）且有 1MB 上限，
// 而 level.dat 是 **gzip 压缩的二进制 NBT**（实测 1.5KB 压缩 / 3KB 解压）——
// 当成字符串读会损坏内容，长度上限也是另一套语义。
//
// 为什么不让面板用 DownloadFile 拉二进制自己解析：
//   - 文件就在节点上，就地解析最省事；
//   - 错误在文件所在的那一端报出来，定位最直接（"level.dat 损坏"而不是"面板解析失败"）；
//   - 面板不必懂 NBT 的二进制细节。
//
// 本 RPC **只读**。改游戏规则走控制台 `/gamerule`：运行中的服务器会把
// level.dat 里的 GameRules 覆写掉，直接改文件等于"改了却没变"。
func (s *Server) ReadWorldInfo(ctx context.Context, req *pb.WorldInfoRequest) (*pb.WorldInfoResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.WorldInfoResponse{Success: false, Error: err.Error()}, nil
	}

	// 1) 定世界目录名：请求 > server.properties 的 level-name > "world"
	world := strings.TrimSpace(req.World)
	if world == "" {
		world = readLevelName(s, req.InstanceId)
	}
	if world == "" {
		world = "world"
	}
	// 世界目录名会被拼进路径，必须挡住 ../ 之类（虽然最终还会过 resolvePath，
	// 但在这里先拒掉能得到更清楚的错误信息）
	if strings.ContainsAny(world, `/\`) || world == "." || world == ".." {
		return &pb.WorldInfoResponse{Success: false, Error: "世界目录名不合法: " + world}, nil
	}

	// resolvePath 会做 safepath 那一套检查（真实路径 + 符号链接），与文件管理面同源
	target, err := resolvePath(dir, filepath.Join(world, "level.dat"))
	if err != nil {
		// 记录实例目录与目标路径：这条链路上"读不到世界信息"最常见的原因是
		// 实例目录布局与预期不同（容器化实例、共享 jar 目录等），
		// 只回一句"权限不足"会让排查从头猜起。
		s.log.Warn("读世界信息：路径解析失败", "instance", req.InstanceId,
			"dir", dir, "world", world)
		return &pb.WorldInfoResponse{
			Success:  false,
			Error:    "无法定位世界文件：" + err.Error(),
			WorldDir: world,
		}, nil
	}
	s.log.Info("读世界信息", "instance", req.InstanceId, "target", target)

	out := &pb.WorldInfoResponse{WorldDir: world}

	fi, err := os.Stat(target)
	if err != nil || fi.IsDir() {
		// 世界还没生成（实例从未启动过）：这不是错误，是"暂无数据"。
		// 返回 success=true + level_dat_found=false，让面板显示得体的空状态，
		// 而不是抛一个 500 让用户以为面板坏了。
		s.log.Info("读世界信息：level.dat 不存在，按空世界返回",
			"instance", req.InstanceId, "target", target, "err", err)
		out.Success = true
		return out, nil
	}
	// 大小上限：真实的 level.dat 只有几 KB；几十 MB 说明这文件不对
	//（或被人塞了东西进来），拒绝解析而不是让解析器去吃它。
	const maxLevelDat = 8 << 20
	if fi.Size() > maxLevelDat {
		return &pb.WorldInfoResponse{
			Success: false,
			Error:   fmt.Sprintf("level.dat 体积异常（%d 字节，上限 %d）", fi.Size(), maxLevelDat),
		}, nil
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return &pb.WorldInfoResponse{Success: false, Error: "读取 level.dat 失败: " + err.Error()}, nil
	}

	// 2) 解析（nbt 包自带 gzip 自动识别与压缩炸弹/深度/长度防护）
	root, err := nbt.ParseFile(raw)
	if err != nil {
		return &pb.WorldInfoResponse{
			Success: false,
			Error:   "level.dat 解析失败（存档可能损坏）: " + err.Error(),
		}, nil
	}
	data := root.Compound("Data")
	if data == nil {
		return &pb.WorldInfoResponse{
			Success: false,
			Error:   "level.dat 结构异常：缺少 Data 节点（不是有效的 Minecraft 存档？）",
		}, nil
	}
	out.LevelDatFound = true

	// ---- 身份 ----
	if v, ok := data.String("LevelName"); ok {
		out.LevelName = v
	} else {
		out.LevelName = world
	}
	// 种子在 1.16+ 移到了 WorldGenSettings.seed（**不在** Data.Seed）。
	// 实测 Paper 1.21.1 就是这里；老存档两种都可能，所以两处都试。
	if wg := data.Compound("WorldGenSettings"); wg != nil {
		if v, ok := wg.Long("seed"); ok {
			out.Seed = strconv.FormatInt(v, 10)
			out.SeedPresent = true
		}
		if v, ok := wg.Byte("generate_features"); ok {
			out.GenerateFeatures = v != 0
		}
		if v, ok := wg.Byte("bonus_chest"); ok {
			out.BonusChest = v != 0
		}
		// 主世界的生成器类型
		if dims := wg.Compound("dimensions"); dims != nil {
			names := []string{}
			for _, d := range dims.Children() {
				names = append(names, d.Name)
			}
			sort.Strings(names)
			out.Dimensions = names
			if ow := dims.Compound("minecraft:overworld"); ow != nil {
				if gen := ow.Compound("generator"); gen != nil {
					if t, ok := gen.String("type"); ok {
						out.Generator = t
					}
				}
			}
		}
	}
	if !out.SeedPresent {
		// 老格式：Data.Seed
		if v, ok := data.Long("Seed"); ok {
			out.Seed = strconv.FormatInt(v, 10)
			out.SeedPresent = true
		}
	}

	// ---- 玩法 ----
	if v, ok := data.Int("GameType"); ok {
		out.GameType = v
	}
	if v, ok := data.Byte("Difficulty"); ok {
		out.Difficulty = int32(v)
	}
	if v, ok := data.Byte("DifficultyLocked"); ok {
		out.DifficultyLocked = v != 0
	}
	if v, ok := data.Byte("hardcore"); ok {
		out.Hardcore = v != 0
	}
	if v, ok := data.Byte("allowCommands"); ok {
		out.AllowCommands = v != 0
	}

	// ---- 出生点与边界 ----
	if v, ok := data.Int("SpawnX"); ok {
		out.SpawnX = v
	}
	if v, ok := data.Int("SpawnY"); ok {
		out.SpawnY = v
	}
	if v, ok := data.Int("SpawnZ"); ok {
		out.SpawnZ = v
	}
	if v, ok := data.Double("BorderSize"); ok {
		out.BorderSize = v
	}
	if v, ok := data.Double("BorderCenterX"); ok {
		out.BorderCenterX = v
	}
	if v, ok := data.Double("BorderCenterZ"); ok {
		out.BorderCenterZ = v
	}

	// ---- 世界状态 ----
	if v, ok := data.Long("Time"); ok {
		out.Time = v
	}
	if v, ok := data.Long("DayTime"); ok {
		out.DayTime = v
	}
	if v, ok := data.Byte("raining"); ok {
		out.Raining = v != 0
	}
	if v, ok := data.Byte("thundering"); ok {
		out.Thundering = v != 0
	}
	// LastPlayed 是**毫秒**（实测 1791210964234），面板显示前要 /1000
	if v, ok := data.Long("LastPlayed"); ok {
		out.LastPlayedMs = v
	}

	// ---- 版本 ----
	if v, ok := data.Int("DataVersion"); ok {
		out.DataVersion = v
	}
	if l := data.List("ServerBrands"); l != nil {
		for _, it := range l.Items() {
			if sv, ok := it.Value.(string); ok {
				out.ServerBrands = append(out.ServerBrands, sv)
			}
		}
	}

	// ---- 规则与数据包 ----
	if req.IncludeGamerules {
		if gr := data.Compound("GameRules"); gr != nil {
			rules := make([]*pb.GameRule, 0, len(gr.Children()))
			for _, c := range gr.Children() {
				// GameRules 的值全是 String（实测 51/51），但万一某个核心写成别的
				// 类型，也要能带出去而不是丢规则 —— 丢规则会让面板显示"少了一条"，
				// 用户会以为规则不存在。
				v := ""
				switch c.Type {
				case nbt.TagString:
					v, _ = c.Value.(string)
				case nbt.TagByte, nbt.TagInt, nbt.TagLong, nbt.TagShort:
					v = fmt.Sprintf("%v", c.Value)
				default:
					v = fmt.Sprintf("%v", c.Value)
				}
				rules = append(rules, &pb.GameRule{Name: c.Name, Value: v})
			}
			sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
			out.Gamerules = rules
		}
	}
	if dp := data.Compound("DataPacks"); dp != nil {
		if l := dp.List("Enabled"); l != nil {
			for _, it := range l.Items() {
				if sv, ok := it.Value.(string); ok {
					out.DatapacksEnabled = append(out.DatapacksEnabled, sv)
				}
			}
		}
		if l := dp.List("Disabled"); l != nil {
			for _, it := range l.Items() {
				if sv, ok := it.Value.(string); ok {
					out.DatapacksDisabled = append(out.DatapacksDisabled, sv)
				}
			}
		}
	}

	// ⚠️ 必须在成功路径上显式置真：`out` 是在上面用 `{WorldDir: world}` 建出来的，
	// proto3 的 bool 默认是 **false**。忘了这一步的表现极具迷惑性 ——
	// 面板收到 Success=false 且 Error 为空，只能报"节点拒绝了但没给原因"，
	// 而日志里一切正常（路径解析成功、文件也读到了）。这个坑已经踩过一次。
	out.Success = true
	return out, nil
}

// readLevelName 从 server.properties 读 level-name（读不到就返回空串）。
//
// 为什么需要它：世界目录名可以由用户在 server.properties 里改，
// 不是永远叫 "world"。硬编码 "world" 会在改过名的实例上"读不到世界"。
func readLevelName(s *Server, instanceID string) string {
	dir, err := s.instanceDir(instanceID)
	if err != nil {
		return ""
	}
	target, err := resolvePath(dir, "server.properties")
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "level-name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
