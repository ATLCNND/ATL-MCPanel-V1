package nodeinstall

import (
	"encoding/binary"
	"testing"
)

// mkELF 造一个最小 ELF 头（只填魔数、EI_DATA 与 e_machine）。
//
// 为什么手搓而不是读真二进制：测试不该依赖特定构建产物，
// 而且这样才能精确覆盖"每种架构 → 期望字符串"的映射，
// 包括大端、未知架构、非 ELF 这些真实产物里不会出现但代码必须处理的分支。
func mkELF(machine uint16, littleEndian bool) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2 // ELFCLASS64
	if littleEndian {
		b[5] = 1
		binary.LittleEndian.PutUint16(b[18:], machine)
	} else {
		b[5] = 2
		binary.BigEndian.PutUint16(b[18:], machine)
	}
	return b
}

func TestBinaryArch(t *testing.T) {
	cases := []struct {
		name string
		bin  []byte
		want string
	}{
		{"amd64", mkELF(62, true), "x86_64"},
		{"arm64", mkELF(183, true), "aarch64"},
		{"arm32", mkELF(40, true), "armv7l"},
		{"386", mkELF(3, true), "i686"},
		{"amd64 大端（不常见但要能处理）", mkELF(62, false), "x86_64"},
		{"不认识的架构（MIPS=8）", mkELF(8, true), ""},
		{"非 ELF（shell 脚本）", []byte("#!/bin/sh\necho hi\n"), ""},
		{"空内容", []byte{}, ""},
		{"太短", []byte{0x7f, 'E'}, ""},
		{"魔数对但 EI_DATA 非法", func() []byte { b := mkELF(62, true); b[5] = 9; return b }(), ""},
	}
	for _, c := range cases {
		if got := binaryArch(c.bin); got != c.want {
			t.Errorf("%s: binaryArch = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestNormalizeUnameArch(t *testing.T) {
	cases := map[string]string{
		"x86_64":   "x86_64",
		"X86_64\n": "x86_64",
		"amd64":    "x86_64",
		"aarch64":  "aarch64",
		"arm64":    "aarch64",
		"armv7l":   "armv7l",
		"i686":     "i686",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeUnameArch(in); got != want {
			t.Errorf("normalizeUnameArch(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestArchToGoArch(t *testing.T) {
	cases := map[string]string{
		"x86_64":  "amd64",
		"aarch64": "arm64",
		"armv7l":  "arm",
	}
	for in, want := range cases {
		if got := archToGoArch(in); got != want {
			t.Errorf("archToGoArch(%q) = %q，期望 %q", in, got, want)
		}
	}
}
