package container

import (
	"math"
	"testing"
)

// docker stats 的回退路径是"容器 cgroup 读不到时的兜底"，也是唯一一处
// 需要解析人类可读输出（`22.31%`、`970.1MiB / 1.5GiB`）的地方。
// 这类解析最容易出的错是**单位**（MiB vs MB 差 4.8%）与被截断的输出，
// 两者在界面上都表现为"数字看着差不多，其实是错的"，所以逐项固定下来。

func TestParseDockerStats(t *testing.T) {
	// 这是 docker 26.1 的真实输出形态（JSON 模板），数值取自
	// 2026-09-29 内测节点上 atl-beta01 的实测输出
	out := `{"BlockIO":"0B / 0B","CPUPerc":"22.31%","Container":"abc123","ID":"abc123","MemPerc":"63.14%","MemUsage":"970MiB / 1.5GiB","Name":"atl-beta01","NetIO":"1.2kB / 3.4kB","PIDs":"11"}`
	u, err := parseDockerStats(out)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if math.Abs(u.CPUPct-22.31) > 1e-9 {
		t.Fatalf("CPU 百分比应为 22.31，得到 %v", u.CPUPct)
	}
	if want := uint64(970 * 1024 * 1024); u.MemBytes != want {
		t.Fatalf("内存应为 %d 字节，得到 %d", want, u.MemBytes)
	}
}

func TestParseDockerStatsUnits(t *testing.T) {
	// 1024 进制与 1000 进制必须区分：混淆会让数值差 4.8%
	cases := []struct {
		in   string
		want uint64
	}{
		{"0B", 0},
		{"512B", 512},
		{"1KiB", 1024},
		{"1MiB", 1024 * 1024},
		{"1GiB", 1024 * 1024 * 1024},
		{"1MB", 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{"2.5MiB", 2621440},
	}
	for _, c := range cases {
		got, err := parseByteSize(c.in)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("解析 %q: want %d got %d", c.in, c.want, got)
		}
	}
}

func TestParseDockerStatsErrors(t *testing.T) {
	// 解析不出来时必须报错：宁可让界面显示"未知"，也不能猜一个数
	bad := []string{
		``,
		`not json`,
		`{"CPUPerc":"--","MemUsage":"970MiB / 1GiB"}`,
		`{"CPUPerc":"22.31%","MemUsage":""}`,
	}
	for _, c := range bad {
		if _, err := parseDockerStats(c); err == nil {
			t.Fatalf("输入 %q 应报错", c)
		}
	}
	// 0B 是合法值（容器刚建、还没吃内存），不能当错误
	if _, err := parseDockerStats(`{"CPUPerc":"22.31%","MemUsage":"0B / 1GiB"}`); err != nil {
		t.Fatalf("0B 是合法值，不应报错: %v", err)
	}
}

func TestParseDockerStatsTakesLastLine(t *testing.T) {
	// docker 偶发会先输出一行警告（例如镜像过期提醒），真正数据在最后一行
	out := "WARNING: something\n" + `{"CPUPerc":"1.5%","MemUsage":"100MiB / 1GiB"}`
	u, err := parseDockerStats(out)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if math.Abs(u.CPUPct-1.5) > 1e-9 {
		t.Fatalf("应取最后一行的数据，得到 %v", u.CPUPct)
	}
	if u.MemBytes != 100*1024*1024 {
		t.Fatalf("内存应为 100MiB，得到 %d", u.MemBytes)
	}
}
