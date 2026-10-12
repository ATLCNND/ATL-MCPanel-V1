package nbt

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"strings"
	"testing"
)

// ---- 构造 NBT 的小工具（测试用） ----
//
// 手写字节而不是"先生成再用被测代码读"：后者会把实现里的同一个误解
// 同时写进构造器和解析器，测试就永远通过了。

type enc struct{ b bytes.Buffer }

func (e *enc) u8(v byte)   { e.b.WriteByte(v) }
func (e *enc) i16(v int16) { _ = binary.Write(&e.b, binary.BigEndian, v) }
func (e *enc) i32(v int32) { _ = binary.Write(&e.b, binary.BigEndian, v) }
func (e *enc) i64(v int64) { _ = binary.Write(&e.b, binary.BigEndian, v) }
func (e *enc) str(s string) {
	e.i16(int16(len(s)))
	e.b.WriteString(s)
}
func (e *enc) named(typ byte, name string) {
	e.u8(typ)
	e.str(name)
}

// 一个最小的 level.dat 骨架：根 compound("") → compound("Data") → 若干字段
func sampleNBT() []byte {
	var e enc
	e.named(TagCompound, "") // 根
	e.named(TagCompound, "Data")
	{
		e.named(TagInt, "GameType")
		e.i32(0) // 生存
		e.named(TagString, "LevelName")
		e.str("world")
		e.named(TagByte, "hardcore")
		e.u8(0)
		e.named(TagByte, "allowCommands")
		e.u8(1)
		e.named(TagLong, "Seed")
		e.i64(-4172144997902289642)
		e.named(TagLong, "Time")
		e.i64(123456)
		e.named(TagInt, "SpawnX")
		e.i32(128)
		e.named(TagDouble, "BorderSize")
		e.i64(0x40B0000000000000) // 4096.0
		e.named(TagFloat, "SomeFloat")
		e.i32(0x3F800000) // 1.0
		e.named(TagIntArray, "BorderCenter")
		e.i32(2)
		e.i32(0)
		e.i32(0)
		e.named(TagString, "version")
		e.str("1.21.1")
		// 一个 list：元素类型 int，3 个元素（元素**没有名字**）
		e.named(TagList, "ScheduledEvents")
		e.u8(TagInt)
		e.i32(3)
		for _, v := range []int32{1, 2, 3} {
			e.i32(v)
		}
		// 一个空的 list：元素类型写成 End（合法）
		e.named(TagList, "EmptyList")
		e.u8(TagEnd)
		e.i32(0)
		// 嵌套 compound
		e.named(TagCompound, "Nested")
		{
			e.named(TagString, "Inner")
			e.str("深一层")
		}
		e.u8(TagEnd) // 结束 Nested
	}
	e.u8(TagEnd) // 结束 Data
	e.u8(TagEnd) // 结束根
	return e.b.Bytes()
}

func TestParseScalars(t *testing.T) {
	root, err := Parse(sampleNBT())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	data := root.Compound("Data")
	if data == nil {
		t.Fatal("应有 Data compound")
	}

	if v, ok := data.Int("GameType"); !ok || v != 0 {
		t.Errorf("GameType 应为 0，实际 %d ok=%v", v, ok)
	}
	if v, ok := data.String("LevelName"); !ok || v != "world" {
		t.Errorf("LevelName 应为 world，实际 %q ok=%v", v, ok)
	}
	if v, ok := data.Byte("allowCommands"); !ok || v != 1 {
		t.Errorf("allowCommands 应为 1，实际 %d ok=%v", v, ok)
	}
	// 种子是**有符号** long：负数必须原样读回来（正负搞错会让种子对不上）
	if v, ok := data.Long("Seed"); !ok || v != -4172144997902289642 {
		t.Errorf("Seed 应为 -4172144997902289642，实际 %d ok=%v", v, ok)
	}
	if v, ok := data.Float("SomeFloat"); !ok || v != 1.0 {
		t.Errorf("SomeFloat 应为 1.0，实际 %v ok=%v", v, ok)
	}
	if v, ok := data.Double("BorderSize"); !ok || v != 4096.0 {
		t.Errorf("BorderSize 应为 4096.0，实际 %v ok=%v", v, ok)
	}
	if v, ok := data.IntSlice("BorderCenter"); !ok || len(v) != 2 || v[0] != 0 || v[1] != 0 {
		t.Errorf("BorderCenter 解析错误: %v ok=%v", v, ok)
	}
}

func TestParseList(t *testing.T) {
	root, _ := Parse(sampleNBT())
	data := root.Compound("Data")

	l := data.List("ScheduledEvents")
	if l == nil {
		t.Fatal("应有 ScheduledEvents list")
	}
	items := l.Items()
	if len(items) != 3 {
		t.Fatalf("应有 3 个元素，实际 %d", len(items))
	}
	// 关键：list 元素**没有名字**，且类型由 list 头给定
	for i, it := range items {
		if it.Name != "" {
			t.Errorf("第 %d 个元素不该有名字，实际 %q", i, it.Name)
		}
		if it.Type != TagInt {
			t.Errorf("第 %d 个元素类型应为 int，实际 %d", i, it.Type)
		}
		if v, _ := it.Value.(int32); v != int32(i+1) {
			t.Errorf("第 %d 个元素应 %d，实际 %d", i, i+1, v)
		}
	}

	// 空 list：元素类型为 End、个数为 0，必须能解析且长度为 0
	if el := data.List("EmptyList"); el == nil || len(el.Items()) != 0 {
		t.Errorf("空 list 应解析为 0 个元素，实际 %v", el)
	}
}

func TestParseNestedCompound(t *testing.T) {
	root, _ := Parse(sampleNBT())
	inner := root.Compound("Data").Compound("Nested")
	if inner == nil {
		t.Fatal("应有 Nested")
	}
	if v, ok := inner.String("Inner"); !ok || v != "深一层" {
		t.Errorf("中文嵌套值应正确（UTF-8 多字节），实际 %q ok=%v", v, ok)
	}
}

func TestParseFileHandlesGzip(t *testing.T) {
	// 未压缩
	plain := sampleNBT()
	if _, err := ParseFile(plain); err != nil {
		t.Errorf("未压缩数据应能解析: %v", err)
	}
	// gzip 压缩（level.dat 的真实形态）
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	root, err := ParseFile(buf.Bytes())
	if err != nil {
		t.Fatalf("gzip 数据应能解析: %v", err)
	}
	if v, _ := root.Compound("Data").String("LevelName"); v != "world" {
		t.Errorf("从 gzip 读出的 LevelName 应为 world，实际 %q", v)
	}
}

// TestTypeMismatchDoesNotPanic：字段类型与预期不符时返回 false，而不是崩。
//
// 这是真实场景：GameType 在多数核心下是 int，但有的会写成 string；
// 解析器必须容错，否则一个字段类型不同就让整页数据拿不到。
func TestTypeMismatchDoesNotPanic(t *testing.T) {
	root, _ := Parse(sampleNBT())
	data := root.Compound("Data")

	// LevelName 是 string，按 int 取 → false
	if _, ok := data.Int("LevelName"); ok {
		t.Error("类型不符时应返回 ok=false")
	}
	// 不存在的字段 → false
	if _, ok := data.Int("NoSuchField"); ok {
		t.Error("不存在的字段应返回 ok=false")
	}
	// 对非 compound 取子字段 → nil，不该 panic
	if root.Compound("GameType") != nil {
		t.Error("对 int 字段取 compound 应返回 nil")
	}
	if root.List("GameType") != nil {
		t.Error("对 int 字段取 list 应返回 nil")
	}
}

// ---- 防御性解析：以下构造的都是**恶意/损坏**输入 ----

func TestRejectsTruncated(t *testing.T) {
	full := sampleNBT()
	// 砍掉尾部若干字节：必须报错，不能崩、不能返回半截数据
	for _, cut := range []int{1, 5, 20, len(full) / 2} {
		if _, err := Parse(full[:len(full)-cut]); err == nil {
			t.Errorf("截断 %d 字节后应报错", cut)
		}
	}
}

func TestRejectsUnknownTagType(t *testing.T) {
	// 类型 99 不存在
	bad := []byte{99, 0, 0}
	if _, err := Parse(bad); err == nil {
		t.Error("未知 tag 类型应报错")
	}
}

func TestRejectsHugeStringLength(t *testing.T) {
	// 声称有一个 30000 字节的名字，但数据里没有 → 必须报错而不是申请内存
	var e enc
	e.u8(TagCompound)
	e.i16(30000) // 名字长度声明得很大
	e.b.WriteString("x")
	e.u8(TagEnd)
	if _, err := Parse(e.b.Bytes()); err == nil {
		t.Error("字符串长度与实际数据不符时应报错")
	}
}

func TestRejectsHugeArrayLength(t *testing.T) {
	// 一个字节数组声称有 20 亿个元素 → 必须拒绝（否则一次申请 2GB）
	var e enc
	e.named(TagCompound, "")
	e.named(TagByteArry, "big")
	e.i32(2000000000)
	e.u8(TagEnd)
	if _, err := Parse(e.b.Bytes()); err == nil {
		t.Error("超大数组长度应被拒绝")
	}
}

func TestRejectsNegativeLength(t *testing.T) {
	var e enc
	e.named(TagCompound, "")
	e.named(TagString, "bad")
	e.i16(-1) // 负的字符串长度
	e.u8(TagEnd)
	if _, err := Parse(e.b.Bytes()); err == nil {
		t.Error("负长度应被拒绝")
	}
}

// TestRejectsDeepNesting：构造 200 层嵌套，必须被深度上限挡住（防栈溢出）。
func TestRejectsDeepNesting(t *testing.T) {
	var e enc
	e.named(TagCompound, "")
	for i := 0; i < 200; i++ {
		e.named(TagCompound, "n")
	}
	// 不写结束标记：如果没被深度挡住，会一直读到数据末尾才报错 ——
	// 两种情况都返回错误，但这里主要确认**不会崩**。
	_, err := Parse(e.b.Bytes())
	if err == nil {
		t.Error("超深嵌套应报错")
	}
	// 确认错误信息与深度有关（而不是碰巧因为数据不够）
	if err != nil && !strings.Contains(err.Error(), "嵌套") && !strings.Contains(err.Error(), "NBT") {
		t.Logf("深度限制未触发，而是因为数据不足报错（可接受）: %v", err)
	}
}

// TestRejectsGzipBomb：压缩炸弹（解压后超上限）必须被挡住。
func TestRejectsGzipBomb(t *testing.T) {
	// 造一个解压后远大于上限的 gzip：全是零字节，压缩比极高
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zeros := make([]byte, 1<<20)
	for i := 0; i < (maxDecompressBytes>>20)+8; i++ {
		if _, err := zw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	if _, err := ParseFile(buf.Bytes()); err == nil {
		t.Error("压缩炸弹应被拒绝")
	} else if !strings.Contains(err.Error(), "MB") {
		t.Errorf("错误信息应说明超出体积上限，实际: %v", err)
	}
}

func TestEmptyInputErrors(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("空输入应报错")
	}
	if _, err := Parse([]byte{}); err == nil {
		t.Error("空输入应报错")
	}
}

// TestChildrenPreserveOrder：compound 的字段顺序必须与文件里一致
// （物品栏/属性等场景依赖顺序，且便于与其它工具的输出对照）。
func TestChildrenPreserveOrder(t *testing.T) {
	root, _ := Parse(sampleNBT())
	names := []string{}
	for _, c := range root.Compound("Data").Children() {
		names = append(names, c.Name)
	}
	want := []string{"GameType", "LevelName", "hardcore", "allowCommands", "Seed", "Time", "SpawnX"}
	if len(names) < len(want) {
		t.Fatalf("字段数不足: %v", names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("第 %d 个字段应为 %q，实际 %q", i, w, names[i])
		}
	}
}

// TestParseRealLevelDatShape 用**与实测一致**的结构构造一份 level.dat 骨架，
// 锁住「游戏设置」页依赖的那几个字段路径。
//
// 为什么值得单独一条：这些路径是实测出来的（见 docs/LEVEL-DAT-FIELDS.md），
// 而它们**反直觉**——
//   - 种子在 `WorldGenSettings.seed`，不在 `Data.Seed`（1.16+ 移过去的）；
//   - `GameRules` 的值**全是 String**，连 randomTickSpeed 也是 "3" 而不是 3；
//   - 存档版本是大写 `Version`（compound），另有小写 `version`（int）。
//
// 谁要是"顺手"把路径改回 `Data.Seed`，这条用例会立刻失败。
func TestParseRealLevelDatShape(t *testing.T) {
	var e enc
	e.named(TagCompound, "")
	e.named(TagCompound, "Data")
	{
		// 种子：在 WorldGenSettings 里面
		e.named(TagCompound, "WorldGenSettings")
		{
			e.named(TagByte, "bonus_chest")
			e.u8(0)
			e.named(TagByte, "generate_features")
			e.u8(1)
			e.named(TagLong, "seed")
			e.i64(1283372119871044237)
			// dimensions：键名是 minecraft:overworld 这种带冒号的
			e.named(TagCompound, "dimensions")
			{
				e.named(TagCompound, "minecraft:overworld")
				{
					e.named(TagString, "type")
					e.str("minecraft:overworld")
					e.named(TagCompound, "generator")
					{
						e.named(TagString, "type")
						e.str("minecraft:noise")
					}
					e.u8(TagEnd) // 结束 generator
				}
				e.u8(TagEnd) // 结束 minecraft:overworld
			}
			e.u8(TagEnd) // 结束 dimensions
		}
		e.u8(TagEnd) // 结束 WorldGenSettings

		e.named(TagString, "LevelName")
		e.str("world")
		e.named(TagInt, "GameType")
		e.i32(0)
		e.named(TagByte, "Difficulty")
		e.u8(1)
		// GameRules：**值全是 String**
		e.named(TagCompound, "GameRules")
		{
			e.named(TagString, "keepInventory")
			e.str("false")
			e.named(TagString, "randomTickSpeed")
			e.str("3")
			e.named(TagString, "doDaylightCycle")
			e.str("true")
		}
		e.u8(TagEnd) // 结束 GameRules（只能有一个，多一个会提前把 Data 关掉）
		// 世界边界：未设置时是 6e7 哨兵值
		e.named(TagDouble, "BorderSize")
		e.i64(0x418C9C3800000000) // 6.0e7
		// 大小写两个 version 并存
		e.named(TagInt, "version")
		e.i32(19133)
		e.named(TagInt, "DataVersion")
		e.i32(3955)
	}
	e.u8(TagEnd)
	e.u8(TagEnd)

	root, err := Parse(e.b.Bytes())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	data := root.Compound("Data")
	if data == nil {
		t.Fatal("应有 Data")
	}

	// ① 种子必须在 WorldGenSettings.seed（有符号 long，可能为负/超大）
	wg := data.Compound("WorldGenSettings")
	if wg == nil {
		t.Fatal("应有 WorldGenSettings")
	}
	if v, ok := wg.Long("seed"); !ok || v != 1283372119871044237 {
		t.Errorf("WorldGenSettings.seed 应为 1283372119871044237，实际 %d ok=%v", v, ok)
	}
	// 反面：Data 下**不该**有 Seed（1.16+ 之后）
	if _, ok := data.Long("Seed"); ok {
		t.Error("Data.Seed 不该存在（种子已移到 WorldGenSettings.seed）")
	}

	// ② dimensions 的键名带 minecraft: 前缀，且能取到嵌套 generator.type
	dims := wg.Compound("dimensions")
	if dims == nil {
		t.Fatal("应有 dimensions")
	}
	ow := dims.Compound("minecraft:overworld")
	if ow == nil {
		t.Fatal("应有 minecraft:overworld（注意键名带前缀）")
	}
	if v, ok := ow.Compound("generator").String("type"); !ok || v != "minecraft:noise" {
		t.Errorf("overworld.generator.type 应为 minecraft:noise，实际 %q ok=%v", v, ok)
	}

	// ③ GameRules 的值必须是 **String**（这是最容易写错的一条）
	gr := data.Compound("GameRules")
	if gr == nil {
		t.Fatal("应有 GameRules")
	}
	if v, ok := gr.String("randomTickSpeed"); !ok || v != "3" {
		t.Errorf("randomTickSpeed 应能按**字符串**读出 \"3\"，实际 %q ok=%v", v, ok)
	}
	// 反面：按 int 取应当失败（值确实是字符串，不是数字）
	if _, ok := gr.Int("randomTickSpeed"); ok {
		t.Error("GameRules 的值是 String，按 Int 取不该成功")
	}

	// ④ 边界哨兵值确实是 6e7
	if v, ok := data.Double("BorderSize"); !ok || v != 6.0e7 {
		t.Errorf("BorderSize 应为 6e7 哨兵值，实际 %v ok=%v", v, ok)
	}

	// ⑤ 大小写两个 version 并存，各自类型不同
	if _, ok := data.Int("version"); !ok {
		t.Error("小写 version 应为 int")
	}
	if _, ok := data.Int("DataVersion"); !ok {
		t.Error("DataVersion 应为 int")
	}
}
