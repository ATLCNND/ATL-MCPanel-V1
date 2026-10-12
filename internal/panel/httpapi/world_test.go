package httpapi

import "testing"

// TestBorderIsSet 覆盖"世界边界是否设置"的判定。
//
// 这条用例来自**真机实测发现的 bug**：某个真实存档里 BorderSize 存成了
// 59999968（不是精确的 6e7），而初版实现用 ±1 容差比较，
// 于是它被判成"已设置"，界面显示"59,999,968 格"——纯属胡说。
//
// 原版语义：未设置边界时写入 6e7 哨兵值；真实边界上限是 ±29999984。
// 所以"接近 6e7"一律算未设置。
func TestBorderIsSet(t *testing.T) {
	cases := []struct {
		size float64
		want bool
		why  string
	}{
		{0, false, "0 表示从未设置"},
		{6.0e7, false, "精确哨兵值"},
		{59999968, false, "实测出现过的哨兵值（浮点偏差，必须也判成未设置）"},
		{59999999.5, false, "哨兵附近的浮点值"},
		{60000000.0001, false, "哨兵附近的浮点值"},
		{29999984 * 2, false, "正好是哨兵附近（不应出现，但按未设置处理）"},
		{29999984, true, "原版边界上限，是真的设置了"},
		{-29999984, true, "负方向的原版上限"},
		{1000, true, "小边界"},
		{100, true, "很小但有效"},
		{1e7, true, "一千万格：小于判据阈值，算已设置"},
		{-100, true, "负值也是有效边界"},
	}
	for _, c := range cases {
		if got := borderIsSet(c.size); got != c.want {
			t.Errorf("borderIsSet(%v) = %v，应为 %v（%s）", c.size, got, c.want, c.why)
		}
	}
}
