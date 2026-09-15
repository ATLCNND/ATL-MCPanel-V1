package monitor

import (
	"regexp"
	"testing"
)

func TestParseList(t *testing.T) {
	cases := []struct {
		line    string
		players int32
		max     int32
		ok      bool
	}{
		{"[19:08:55 INFO]: There are 0 of a max of 20 players online: ", 0, 20, true},
		{"[19:08:55 INFO]: There are 3 of a max of 100 players online: Alice, Bob, Carol", 3, 100, true},
		{"§6There are 7 of a max of 20 players online: §rA", 7, 20, true},
		{"无关输出", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		p, m, ok := parseList(c.line)
		if ok != c.ok {
			t.Errorf("parseList(%q) ok = %v，期望 %v", c.line, ok, c.ok)
			continue
		}
		if ok && (p != c.players || m != c.max) {
			t.Errorf("parseList(%q) = (%d,%d)，期望 (%d,%d)", c.line, p, m, c.players, c.max)
		}
	}
}

func TestParsePlayerNames(t *testing.T) {
	names := parsePlayerNames("There are 2 of a max of 20 players online: Alice, Bob")
	if len(names) != 2 {
		t.Fatalf("应解析出 2 个玩家，实际 %d", len(names))
	}
	if _, ok := names["Alice"]; !ok {
		t.Error("应包含 Alice")
	}
	if _, ok := names["Bob"]; !ok {
		t.Error("应包含 Bob")
	}
	// 无玩家
	if n := parsePlayerNames("There are 0 of a max of 20 players online: "); len(n) != 0 {
		t.Errorf("无玩家时应为空，实际 %v", n)
	}
	// 非 list 输出
	if n := parsePlayerNames("随便一行"); len(n) != 0 {
		t.Errorf("非 list 输出应为空，实际 %v", n)
	}
}

func TestMatchFloat(t *testing.T) {
	tests := []struct {
		re   *regexp.Regexp
		line string
		want float64
		ok   bool
	}{
		{tpsPaperRe, "[INFO]: TPS from last 1m, 5m, 15m: 20.0, 19.9, 19.8", 20.0, true},
		{tpsFoliaRe, " - Median Region TPS: 18.96", 18.96, true},
		{tpsFoliaLo, " - Lowest Region TPS: 17.5", 17.5, true},
		{tpsFoliaRe, "无关输出", 0, false},
	}
	for _, c := range tests {
		got, ok := matchFloat(c.re, c.line)
		if ok != c.ok {
			t.Errorf("matchFloat(%q) ok = %v，期望 %v", c.line, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("matchFloat(%q) = %v，期望 %v", c.line, got, c.want)
		}
	}
}

func TestPlayerTrackingViaEvents(t *testing.T) {
	p := NewPoller(nil, NewStore(), nil)
	st := &instanceState{players: make(map[string]struct{}), stop: make(chan struct{})}

	p.parseLine(st, "[19:00:00 INFO]: Alice joined the game")
	p.parseLine(st, "[19:00:05 INFO]: Bob joined the game")
	if len(st.players) != 2 {
		t.Fatalf("应有 2 名玩家，实际 %d", len(st.players))
	}

	p.parseLine(st, "[19:01:00 INFO]: Alice left the game")
	if len(st.players) != 1 {
		t.Fatalf("应有 1 名玩家，实际 %d", len(st.players))
	}
	if _, ok := st.players["Bob"]; !ok {
		t.Error("Bob 应仍在线")
	}

	// list 输出会校准集合
	p.parseLine(st, "There are 1 of a max of 20 players online: Carol")
	if len(st.players) != 1 {
		t.Errorf("list 应校准为 1 名玩家，实际 %d", len(st.players))
	}
	if _, ok := st.players["Carol"]; !ok {
		t.Error("校准后应包含 Carol")
	}
	if st.maxPlayers != 20 {
		t.Errorf("玩家上限应为 20，实际 %d", st.maxPlayers)
	}
}

func TestStatsStore(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get("x"); ok {
		t.Error("初始应无数据")
	}
	s.Set("x", Stats{Players: 3, MaxPlayers: 20, TPS: 19.9})
	got, ok := s.Get("x")
	if !ok || got.Players != 3 || got.MaxPlayers != 20 || got.TPS != 19.9 {
		t.Errorf("读取数据不匹配: %+v ok=%v", got, ok)
	}
	s.Forget("x")
	if _, ok := s.Get("x"); ok {
		t.Error("Forget 后应无数据")
	}
}
