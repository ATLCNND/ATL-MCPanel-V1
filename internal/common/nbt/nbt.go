// Package nbt 解析 Minecraft 的 NBT（Named Binary Tag）格式。
//
// 为什么自己写而不是引第三方：需要的东西很窄 —— **只读**、只要 level.dat 与
// playerdata/*.dat 里的那几种 tag。引一个库进来要跟着它的版本走、还要审它的
// 依赖；而 NBT 的规范本身很小（13 种 tag + 两个容器类型），几百行能写完并且
// 可控。这与本仓库其它地方"能用标准库就不引依赖"的口径一致。
//
// ⚠️ 这是**只读**实现。写入 NBT（比如改玩家物品栏）需要另一套东西：
// 必须保持 tag 顺序与类型、并正确处理 gzip 重写，风险高得多，
// 真有需要时单独设计，不要在这个包上随手加个 Write。
//
// 格式要点（1.21.x 仍然适用）：
//
//	文件 = gzip( 根 TAG_Compound )
//	根通常是 TAG_Compound("") ，里面一个 TAG_Compound("Data") 才是载荷。
//
//	tag 的二进制形态：[类型 1 字节][名字长度 2 字节大端][名字 UTF-8][载荷]
//	（TAG_End=0 没有名字与载荷，只用在一个 compound 的结尾）
package nbt

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// 13 种 tag 类型（数值与官方规范一致，不能改）。
const (
	TagEnd       = 0
	TagByte      = 1
	TagShort     = 2
	TagInt       = 3
	TagLong      = 4
	TagFloat     = 5
	TagDouble    = 6
	TagByteArry  = 7 // 官方写作 TAG_Byte_Array
	TagString    = 8
	TagList      = 9
	TagCompound  = 10
	TagIntArray  = 11
	TagLongArray = 12
)

// 解析时的保护上限。
//
// 这些不是"性能优化"，而是**防御**：level.dat 与 playerdata 理论上来自租户可控的
// 实例目录（他们能往里塞文件），而一个精心构造的 length 前缀能让解析器申请
// 数 GB 内存或递归到栈溢出 —— 那会把 Daemon 拖死，进而影响同节点其它实例。
// 正常文件远小于这些值（实测 level.dat 解压后仅 3KB）。
const (
	maxDecompressBytes = 64 << 20 // gzip 解压后的上限（64MB）
	maxDepth           = 64       // 容器嵌套深度
	maxListLen         = 1 << 22  // 单个 list/array 的元素数上限（约 400 万）
	maxStringLen       = 1 << 20  // 单个字符串长度上限（1MB）
	maxTotalElements   = 1 << 24  // 一次解析的累计元素数上限
)

// Tag 是解析后的一个 NBT 节点。
//
// 用统一结构而不是给每种类型一个 Go 类型：调用方读 level.dat 时是按名字取字段的
// （`root.Compound("Data").String("LevelName")`），值类型在这里是次要信息。
type Tag struct {
	Type  byte
	Name  string
	Value any // 具体类型见各访问器；List 为 []*Tag，Compound 为 []*Tag
}

// Compound 按名字取子 compound；不存在或类型不符时返回 nil。
func (t *Tag) Compound(name string) *Tag {
	c := t.child(name)
	if c == nil || c.Type != TagCompound {
		return nil
	}
	return c
}

// List 按名字取子 list；不存在或类型不符时返回 nil。
func (t *Tag) List(name string) *Tag {
	c := t.child(name)
	if c == nil || c.Type != TagList {
		return nil
	}
	return c
}

// child 在 compound 里按名字查找。仅对 compound 有意义。
func (t *Tag) child(name string) *Tag {
	if t == nil || t.Type != TagCompound {
		return nil
	}
	items, _ := t.Value.([]*Tag)
	for _, it := range items {
		if it.Name == name {
			return it
		}
	}
	return nil
}

// Children 返回 compound 的所有子节点（顺序为文件里的原始顺序）。
func (t *Tag) Children() []*Tag {
	if t == nil || t.Type != TagCompound {
		return nil
	}
	items, _ := t.Value.([]*Tag)
	return items
}

// Items 返回 list 的元素。list 的元素**没有名字**（名称为空字符串）。
func (t *Tag) Items() []*Tag {
	if t == nil || t.Type != TagList {
		return nil
	}
	items, _ := t.Value.([]*Tag)
	return items
}

// ---- 标量取值：类型不符时返回零值与 false ----
//
// 为什么用 (值, ok) 而不是 panic 或直接返回零值：level.dat 的字段在不同
// 版本/不同核心下类型可能不同（例如 GameType 是 int，但某些情况是 string）。
// 调用方需要能区分"没有这个字段/类型不对"和"值就是 0"。

func (t *Tag) Int(name string) (int32, bool) {
	c := t.child(name)
	if c == nil || c.Type != TagInt {
		return 0, false
	}
	v, ok := c.Value.(int32)
	return v, ok
}

func (t *Tag) Long(name string) (int64, bool) {
	c := t.child(name)
	if c == nil {
		return 0, false
	}
	switch c.Type {
	case TagLong:
		v, ok := c.Value.(int64)
		return v, ok
	case TagInt: // 有些核心把 long 写成 int
		v, ok := c.Value.(int32)
		return int64(v), ok
	}
	return 0, false
}

func (t *Tag) Byte(name string) (int8, bool) {
	c := t.child(name)
	if c == nil || c.Type != TagByte {
		return 0, false
	}
	v, ok := c.Value.(int8)
	return v, ok
}

func (t *Tag) Float(name string) (float32, bool) {
	c := t.child(name)
	if c == nil || c.Type != TagFloat {
		return 0, false
	}
	v, ok := c.Value.(float32)
	return v, ok
}

func (t *Tag) Double(name string) (float64, bool) {
	c := t.child(name)
	if c == nil || c.Type != TagDouble {
		return 0, false
	}
	v, ok := c.Value.(float64)
	return v, ok
}

// String 取字符串字段。
//
// 容错：有些字段在某些核心下会是 int（例如 GameType 时而 int 时而 string），
// 所以类型不符时**不报错**，返回 ("", false)，由调用方决定怎么显示。
func (t *Tag) String(name string) (string, bool) {
	c := t.child(name)
	if c == nil || c.Type != TagString {
		return "", false
	}
	v, ok := c.Value.(string)
	return v, ok
}

// IntSlice 取 TAG_Int_Array（世界边界等会用到）。
func (t *Tag) IntSlice(name string) ([]int32, bool) {
	c := t.child(name)
	if c == nil || c.Type != TagIntArray {
		return nil, false
	}
	v, ok := c.Value.([]int32)
	return v, ok
}

// ---- 解析入口 ----

// Parse 解析一段**未压缩**的 NBT 数据。
func Parse(data []byte) (*Tag, error) {
	r := &reader{data: data, total: 0}
	tag, err := r.readTag(0, true)
	if err != nil {
		return nil, err
	}
	if tag == nil {
		return nil, errors.New("空的 NBT 数据")
	}
	return tag, nil
}

// ParseFile 解析一个 NBT 文件，自动识别 gzip。
//
// 为什么要自动识别：level.dat 是 gzip 的（实测 `1f 8b` 开头），而 playerdata/*.dat
// 同样是 gzip；但某些工具产出的 .dat 可能是未压缩的裸 NBT。两种都收，
// 免得调用方要为"这个文件压没压"写分支。
func ParseFile(data []byte) (*Tag, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip 头无效: %w", err)
		}
		defer zr.Close()
		// 限制解压后的体积：压缩炸弹（几 KB 解出几 GB）在这里被挡住。
		raw, err := io.ReadAll(io.LimitReader(zr, maxDecompressBytes+1))
		if err != nil {
			return nil, fmt.Errorf("解压失败: %w", err)
		}
		if len(raw) > maxDecompressBytes {
			return nil, fmt.Errorf("解压后超过 %d MB，拒绝解析", maxDecompressBytes>>20)
		}
		return Parse(raw)
	}
	return Parse(data)
}

// ---- 内部读取器 ----

type reader struct {
	data  []byte
	pos   int
	total int // 累计读出的元素数（防炸弹）
}

var errTruncated = errors.New("NBT 数据在预期结束前就没了（文件可能损坏）")

func (r *reader) need(n int) error {
	if n < 0 || r.pos+n > len(r.data) {
		return errTruncated
	}
	return nil
}

func (r *reader) u8() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *reader) i16() (int16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := int16(binary.BigEndian.Uint16(r.data[r.pos:]))
	r.pos += 2
	return v, nil
}

func (r *reader) i32() (int32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := int32(binary.BigEndian.Uint32(r.data[r.pos:]))
	r.pos += 4
	return v, nil
}

func (r *reader) i64() (int64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := int64(binary.BigEndian.Uint64(r.data[r.pos:]))
	r.pos += 8
	return v, nil
}

// readString 读 [长度 2 字节][UTF-8 内容]。
func (r *reader) readString() (string, error) {
	n, err := r.i16()
	if err != nil {
		return "", err
	}
	if n < 0 {
		return "", errors.New("字符串长度为负，数据已损坏")
	}
	if int(n) > maxStringLen {
		return "", fmt.Errorf("字符串过长（%d 字节），拒绝解析", n)
	}
	if err := r.need(int(n)); err != nil {
		return "", err
	}
	b := r.data[r.pos : r.pos+int(n)]
	r.pos += int(n)
	// 名字/字符串理论上必须是合法 UTF-8。不合法时用替换字符而不是报错 ——
	// 实例里的文件是租户可控的，为一个畸形名字让整个解析失败不划算。
	if !utf8.Valid(b) {
		return string(bytes.ToValidUTF8(b, []byte("\uFFFD"))), nil
	}
	return string(b), nil
}

// readTag 读一个 tag。
//
// withName=false 用于 list 的元素：list 里的元素只有类型与载荷，**没有名字**。
// 这个区别很容易写错 —— 写成有名字的话，list 解析会整体错位。
func (r *reader) readTag(depth int, withName bool) (*Tag, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("嵌套超过 %d 层，拒绝解析", maxDepth)
	}
	r.total++
	if r.total > maxTotalElements {
		return nil, errors.New("元素总数过多，拒绝解析")
	}

	typ, err := r.u8()
	if err != nil {
		return nil, err
	}
	if typ == TagEnd {
		return nil, nil // 由调用方（compound/list 循环）判断结束
	}

	var name string
	if withName {
		if name, err = r.readString(); err != nil {
			return nil, err
		}
	}

	t := &Tag{Type: typ, Name: name}
	switch typ {
	case TagByte:
		b, err := r.u8()
		if err != nil {
			return nil, err
		}
		t.Value = int8(b)
	case TagShort:
		v, err := r.i16()
		if err != nil {
			return nil, err
		}
		t.Value = v
	case TagInt:
		v, err := r.i32()
		if err != nil {
			return nil, err
		}
		t.Value = v
	case TagLong:
		v, err := r.i64()
		if err != nil {
			return nil, err
		}
		t.Value = v
	case TagFloat:
		v, err := r.i32()
		if err != nil {
			return nil, err
		}
		t.Value = math.Float32frombits(uint32(v))
	case TagDouble:
		v, err := r.i64()
		if err != nil {
			return nil, err
		}
		t.Value = math.Float64frombits(uint64(v))
	case TagByteArry:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > maxListLen*8 {
			return nil, fmt.Errorf("字节数组长度异常（%d）", n)
		}
		if err := r.need(int(n)); err != nil {
			return nil, err
		}
		b := make([]byte, n)
		copy(b, r.data[r.pos:r.pos+int(n)])
		r.pos += int(n)
		t.Value = b
	case TagString:
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		t.Value = s
	case TagList:
		items, err := r.readList(depth)
		if err != nil {
			return nil, err
		}
		t.Value = items
	case TagCompound:
		items, err := r.readCompound(depth)
		if err != nil {
			return nil, err
		}
		t.Value = items
	case TagIntArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > maxListLen {
			return nil, fmt.Errorf("int 数组长度异常（%d）", n)
		}
		arr := make([]int32, n)
		for i := range arr {
			v, err := r.i32()
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		t.Value = arr
	case TagLongArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > maxListLen {
			return nil, fmt.Errorf("long 数组长度异常（%d）", n)
		}
		arr := make([]int64, n)
		for i := range arr {
			v, err := r.i64()
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		t.Value = arr
	default:
		return nil, fmt.Errorf("未知的 tag 类型 %d", typ)
	}
	return t, nil
}

// readCompound 读一个 compound 的所有子 tag，直到遇到 TAG_End。
func (r *reader) readCompound(depth int) ([]*Tag, error) {
	var items []*Tag
	for {
		// 先探一个类型字节判断是不是 TAG_End（TAG_End 没有名字与载荷）
		if err := r.need(1); err != nil {
			return nil, err
		}
		if r.data[r.pos] == TagEnd {
			r.pos++
			return items, nil
		}
		child, err := r.readTag(depth+1, true)
		if err != nil {
			return nil, err
		}
		if child == nil {
			return items, nil
		}
		items = append(items, child)
	}
}

// readList 读一个 list：先是元素类型 1 字节，再是元素个数 4 字节，然后逐个元素。
//
// 注意：**元素类型由 list 头统一指定**，元素自己不写类型字节，也没有名字。
// 空 list（个数为 0）的元素类型经常写成 TAG_End，这是合法的。
func (r *reader) readList(depth int) ([]*Tag, error) {
	elemType, err := r.u8()
	if err != nil {
		return nil, err
	}
	n, err := r.i32()
	if err != nil {
		return nil, err
	}
	if n < 0 || int(n) > maxListLen {
		return nil, fmt.Errorf("list 元素数异常（%d）", n)
	}
	if n == 0 {
		return nil, nil
	}
	items := make([]*Tag, 0, n)
	for i := int32(0); i < n; i++ {
		r.total++
		if r.total > maxTotalElements {
			return nil, errors.New("元素总数过多，拒绝解析")
		}
		if depth+1 > maxDepth {
			return nil, fmt.Errorf("嵌套超过 %d 层，拒绝解析", maxDepth)
		}
		// 用带类型的低层读取：list 元素没有名字
		t, err := r.readTagWithType(elemType, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, nil
}

// readTagWithType 按**已知类型**读一个无名字的 tag（list 元素用）。
//
// 单独写一个而不是给 readTag 加参数：list 的走向是"类型已知、跳过类型字节"，
// 与 compound 的"先读类型"是两条不同路径，混在一起容易把 list 解析写错位。
func (r *reader) readTagWithType(typ byte, depth int) (*Tag, error) {
	t := &Tag{Type: typ}
	switch typ {
	case TagByte:
		b, err := r.u8()
		if err != nil {
			return nil, err
		}
		t.Value = int8(b)
	case TagShort:
		v, err := r.i16()
		if err != nil {
			return nil, err
		}
		t.Value = v
	case TagInt:
		v, err := r.i32()
		if err != nil {
			return nil, err
		}
		t.Value = v
	case TagLong:
		v, err := r.i64()
		if err != nil {
			return nil, err
		}
		t.Value = v
	case TagFloat:
		v, err := r.i32()
		if err != nil {
			return nil, err
		}
		t.Value = math.Float32frombits(uint32(v))
	case TagDouble:
		v, err := r.i64()
		if err != nil {
			return nil, err
		}
		t.Value = math.Float64frombits(uint64(v))
	case TagByteArry:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > maxListLen*8 {
			return nil, fmt.Errorf("字节数组长度异常（%d）", n)
		}
		if err := r.need(int(n)); err != nil {
			return nil, err
		}
		b := make([]byte, n)
		copy(b, r.data[r.pos:r.pos+int(n)])
		r.pos += int(n)
		t.Value = b
	case TagString:
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		t.Value = s
	case TagList:
		items, err := r.readList(depth)
		if err != nil {
			return nil, err
		}
		t.Value = items
	case TagCompound:
		items, err := r.readCompound(depth)
		if err != nil {
			return nil, err
		}
		t.Value = items
	case TagIntArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > maxListLen {
			return nil, fmt.Errorf("int 数组长度异常（%d）", n)
		}
		arr := make([]int32, n)
		for i := range arr {
			v, err := r.i32()
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		t.Value = arr
	case TagLongArray:
		n, err := r.i32()
		if err != nil {
			return nil, err
		}
		if n < 0 || int(n) > maxListLen {
			return nil, fmt.Errorf("long 数组长度异常（%d）", n)
		}
		arr := make([]int64, n)
		for i := range arr {
			v, err := r.i64()
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		t.Value = arr
	default:
		return nil, fmt.Errorf("list 元素类型非法（%d）", typ)
	}
	return t, nil
}
