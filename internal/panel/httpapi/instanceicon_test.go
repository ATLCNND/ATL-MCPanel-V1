package httpapi

import "testing"

// 图标的 Content-Type 按**内容**判断，不按文件名 ——
// 用户把 jpg 改名成 icon.png 是常见操作，而报错类型会让浏览器直接不渲染。
func TestSniffImageType(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"PNG", append(png, 0x00), "image/png"},
		{"JPEG", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}, "image/jpeg"},
		{"GIF87a", []byte("GIF87a\x01\x00"), "image/gif"},
		{"GIF89a", []byte("GIF89a\x01\x00"), "image/gif"},
		{"WebP", append(append([]byte("RIFF"), 0x24, 0x00, 0x00, 0x00), []byte("WEBPVP8 ")...), "image/webp"},
		{"纯文本", []byte("this is not an image at all"), ""},
		{"空", nil, ""},
		{"太短", []byte{0x89, 'P'}, ""},
		// RIFF 但不是 WEBP（例如 wav）要拒绝
		{"RIFF 非 WebP", append(append([]byte("RIFF"), 0x24, 0x00, 0x00, 0x00), []byte("WAVEfmt ")...), ""},
	}
	for _, c := range cases {
		if got := sniffImageType(c.data); got != c.want {
			t.Errorf("%s: sniffImageType = %q，期望 %q", c.name, got, c.want)
		}
	}
}
