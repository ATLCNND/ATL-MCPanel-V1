// Package fileops 实现实例目录内的重 IO 文件操作：打包 / 解包 / 复制。
//
// 这些操作与 grpcapi 中的普通文件接口分开，原因有二：
//  1. 它们可能持续数分钟（几个 GB 的世界存档），必须走排队任务而非同步 RPC；
//  2. 它们需要进度回调，而 grpcapi 的同步接口没有回传进度的位置。
//
// 本包不感知「实例」概念，只处理绝对路径；调用方（grpcapi）负责把
// 实例相对路径解析为绝对路径并做越权/受保护文件过滤。
package fileops

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Report 进度回调。done/total 为字节数（total 为 0 表示未知）。
type Report func(progress int, message string, done, total int64)

// Filter 过滤单个文件；返回 false 表示跳过（如面板内部凭据文件）。
// name 为相对归档根的斜杠路径。
type Filter func(name string) bool

// 解包的资源上限。
//
// 压缩包是用户可控输入，zip 炸弹（几 KB 解开几个 TB）会把节点磁盘打满，
// 连带影响同节点其它实例。因此在解包过程中实时校验，而不是事后检查。
const (
	maxExtractBytes  = int64(200) << 30 // 200 GB
	maxExtractFiles  = 500000           // 条目数上限
	maxExtractDepth  = 64               // 目录层级上限
	compressBufSize  = 1 << 20          // 1 MB 缓冲区
)

// ErrUnsupported 不支持的归档格式。
var ErrUnsupported = errors.New("不支持的压缩格式（仅支持 zip / tar.gz / tar）")

// FormatFromName 按文件名推断格式。返回空串表示无法推断。
func FormatFromName(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return "zip"
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(l, ".tar"):
		return "tar"
	}
	return ""
}

// DefaultExt 返回格式对应的默认扩展名。
func DefaultExt(format string) string {
	switch format {
	case "tar.gz":
		return ".tar.gz"
	case "tar":
		return ".tar"
	default:
		return ".zip"
	}
}

// ---- 打包 ----

// Compress 把 src 打包为 dst。
//
// 若 src 是目录，归档内会保留该目录名本身（与 `zip -r out.zip dir` 一致），
// 这样解包后能还原出原目录结构，而不是把内容撒在当前目录里。
func Compress(ctx context.Context, src, dst, format string, filter Filter, report Report) error {
	if format == "" {
		format = FormatFromName(dst)
	}

	// 先统计总量，进度才有意义（否则只能显示"处理中"）；
	// measure 同时承担"源路径是否存在"的校验（不存在时返回错误）
	total, err := measure(src, dst, filter)
	if err != nil {
		return err
	}
	if report != nil {
		report(0, "正在打包…", 0, total)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part" // 先写临时文件：中途失败不会留下"看起来正常"的半截压缩包
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		os.Remove(tmp) // 成功路径上已被 Rename 掉，这里只会删除失败残留
	}()

	var writeErr error
	switch format {
	case "zip":
		writeErr = compressZip(ctx, out, src, dst, filter, total, report)
	case "tar.gz":
		writeErr = compressTar(ctx, out, src, dst, filter, total, report, true)
	case "tar":
		writeErr = compressTar(ctx, out, src, dst, filter, total, report, false)
	default:
		return ErrUnsupported
	}
	if writeErr != nil {
		return writeErr
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// measure 统计待打包的总字节数。
//
// 需要排除输出文件自身：把压缩包写在源目录内是很自然的做法
//（"把这个世界打包到它旁边"），而打包过程中那个文件正在被写入 ——
// 把它算进总量会让进度算错，若再被 Walk 读到还会把"半个自己"塞进归档。
func measure(src, dst string, filter Filter) (int64, error) {
	var total int64
	info, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	err = filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个文件不可读不应中断整个打包
		}
		if fi.IsDir() || isSelfOutput(p, dst) {
			return nil
		}
		if filter != nil && !filter(filepath.ToSlash(p)) {
			return nil
		}
		total += fi.Size()
		return nil
	})
	return total, err
}

// isSelfOutput 判断某个路径是否就是本次压缩的输出（含写盘中的 .part 临时文件）。
func isSelfOutput(p, dst string) bool {
	if dst == "" {
		return false
	}
	a, _ := filepath.Abs(p)
	b, _ := filepath.Abs(dst)
	return a == b || a == b+".part"
}

func compressZip(ctx context.Context, out io.Writer, src, dst string, filter Filter, total int64, report Report) error {
	zw := zip.NewWriter(out)
	defer zw.Close()

	base := filepath.Dir(src)
	var done int64
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 跳过正在写入的输出文件本身（见 measure 的说明）
		if isSelfOutput(p, dst) {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filter != nil && !filter(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		hdr.Name = rel
		hdr.Method = zip.Deflate
		if fi.IsDir() {
			hdr.Name += "/"
			_, err = zw.CreateHeader(hdr)
			return err
		}

		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		n, err := copyFile(p, w)
		done += n
		if report != nil {
			report(percent(done, total), "正在打包 "+rel, done, total)
		}
		return err
	})
	return err
}

func compressTar(ctx context.Context, out io.Writer, src, dst string, filter Filter, total int64, report Report, gz bool) error {
	var w io.Writer = out
	var gzw *gzip.Writer
	if gz {
		// BestSpeed：压缩这类操作是 CPU 密集的，而面板上同时还有游戏的 tick 循环，
		// 用默认级别会把节点的 CPU 抢得很明显，得不偿失。
		gzw, _ = gzip.NewWriterLevel(out, gzip.BestSpeed)
		defer gzw.Close()
		w = gzw
	}
	tw := tar.NewWriter(w)
	defer tw.Close()

	base := filepath.Dir(src)
	var done int64
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 跳过正在写入的输出文件本身（见 measure 的说明）
		if isSelfOutput(p, dst) {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filter != nil && !filter(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		// 归档内不保留原始 uid/gid/属主：解包到别的实例时只会带来困惑
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		n, err := copyFile(p, tw)
		done += n
		if report != nil {
			report(percent(done, total), "正在打包 "+rel, done, total)
		}
		return err
	})
	return err
}

// ---- 解包 ----

// Extract 把压缩包 src 解到目录 dst（dst 会被创建）。
//
// 安全措施：
//   - 拒绝绝对路径与包含 ".." 的条目（zip-slip / tar-slip）
//   - 拒绝符号链接与硬链接（可借链接指向实例目录之外）
//   - 限制总解包字节数、条目数与目录深度（zip 炸弹）
func Extract(ctx context.Context, src, dst, format string, filter Filter, report Report) error {
	if format == "" {
		format = FormatFromName(src)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	magic = magic[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// 以「文件内容」而非扩展名判断格式：用户常把 .tar.gz 命名成 .zip，
	// 或反过来；按魔数走才不会莫名失败。（format 仅用于魔数无法判断时兜底）
	switch {
	case len(magic) >= 2 && magic[0] == 'P' && magic[1] == 'K':
		format = "zip"
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		format = "tar.gz"
	default:
		// tar 没有魔数，只能按扩展名判断；无法判断时沿用调用方传入的 format
		if format == "" {
			format = FormatFromName(src)
		}
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if report != nil {
		report(0, "正在解包…", 0, 0)
	}

	switch format {
	case "zip":
		return extractZip(ctx, src, dst, filter, report)
	case "tar.gz", "tar":
		return extractTar(ctx, f, dst, format == "tar.gz", filter, report)
	default:
		return ErrUnsupported
	}
}

func extractZip(ctx context.Context, src, dst string, filter Filter, report Report) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer zr.Close()

	if len(zr.File) > maxExtractFiles {
		return fmt.Errorf("压缩包条目过多（%d > %d）", len(zr.File), maxExtractFiles)
	}
	// 先累加声明的大小，能低成本挡掉绝大多数 zip 炸弹
	var declared int64
	for _, f := range zr.File {
		declared += int64(f.UncompressedSize64)
	}
	if declared > maxExtractBytes {
		return fmt.Errorf("解包后体积过大（约 %d GB）", declared>>30)
	}

	var done int64
	for _, f := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}
		if filter != nil && !filter(filepath.ToSlash(f.Name)) {
			continue
		}

		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			continue // 跳过符号链接（见 Extract 注释）
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		perm := mode.Perm()
		if perm == 0 {
			perm = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxExtractBytes-done+1))
		out.Close()
		rc.Close()
		done += n
		if done > maxExtractBytes {
			return fmt.Errorf("解包体积超过上限（%d GB）", maxExtractBytes>>30)
		}
		if err != nil {
			return err
		}
		if report != nil {
			report(zipPercent(done, declared), "正在解包 "+f.Name, done, declared)
		}
	}
	return nil
}

func extractTar(ctx context.Context, f *os.File, dst string, gz bool, filter Filter, report Report) error {
	var r io.Reader = f
	if gz {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("打开 gzip 失败: %w", err)
		}
		defer gzr.Close()
		r = gzr
	}
	tr := tar.NewReader(r)

	var done, total int64
	count := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
		if count > maxExtractFiles {
			return fmt.Errorf("压缩包条目过多（> %d）", maxExtractFiles)
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		if filter != nil && !filter(filepath.ToSlash(hdr.Name)) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxExtractBytes {
				return fmt.Errorf("解包体积超过上限（%d GB）", maxExtractBytes>>30)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			perm := os.FileMode(hdr.Mode).Perm()
			if perm == 0 {
				perm = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, maxExtractBytes-done+1))
			out.Close()
			done += n
			if err != nil {
				return err
			}
			if report != nil {
				report(percent(done, total), "正在解包 "+hdr.Name, done, total)
			}
		default:
			// 符号链接 / 硬链接 / 设备文件一律跳过
			continue
		}
	}
	return nil
}

// safeJoin 把归档内的相对路径安全地拼到 dst 下。
//
// 这里**逐段**校验而不是先 filepath.Clean：Clean 会把 "a/../b" 折叠成 "b"，
// 看起来"安全"了，实际却把越界条目悄悄放进目标目录；
// 而 ".." 出现在中间段与出现在开头段的语义完全不同，必须显式拒绝。
func safeJoin(dst, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return "", errors.New("压缩包内含空路径条目")
	}

	parts := strings.Split(name, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("压缩包内含越界路径（%s）", name)
		}
		// 冒号用于 Windows 盘符（C:）、NUL 会截断系统调用参数
		if strings.ContainsAny(p, ":\x00") {
			return "", fmt.Errorf("压缩包内含非法文件名（%s）", name)
		}
		segs = append(segs, p)
	}
	if len(segs) == 0 {
		return "", errors.New("压缩包内含空路径条目")
	}
	if len(segs) > maxExtractDepth {
		return "", fmt.Errorf("压缩包目录层级过深（%s）", name)
	}

	target := filepath.Join(append([]string{dst}, segs...)...)

	// 双保险：即便上面的逐段校验被绕过，最终路径也必须落在 dst 内
	base, _ := filepath.Abs(dst)
	abs, _ := filepath.Abs(target)
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", fmt.Errorf("压缩包内含越界路径（%s）", name)
	}
	return abs, nil
}

// ---- 复制 / 移动 ----

// Copy 递归复制文件或目录。overwrite 为 false 且目标存在时返回错误。
func Copy(ctx context.Context, src, dst string, overwrite bool, report Report) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil && !overwrite {
		return fmt.Errorf("目标已存在：%s", filepath.Base(dst))
	}

	total, _ := measure(src, "", nil)
	var done int64

	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		n, err := copyFile(src, out)
		done += n
		if report != nil {
			report(percent(done, total), "正在复制…", done, total)
		}
		return err
	}

	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil // 不复制符号链接，避免把指向实例目录外的链接带进来
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
		if err != nil {
			return err
		}
		n, err := copyFile(p, out)
		out.Close()
		done += n
		if report != nil {
			report(percent(done, total), "正在复制 "+rel, done, total)
		}
		return err
	})
}

// Move 移动文件或目录。同分区走 Rename，跨分区自动退化为「复制 + 删除」。
func Move(ctx context.Context, src, dst string, overwrite bool, report Report) error {
	if _, err := os.Stat(dst); err == nil {
		if !overwrite {
			return fmt.Errorf("目标已存在：%s", filepath.Base(dst))
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		if report != nil {
			report(100, "移动完成", 0, 0)
		}
		return nil
	}
	// 跨设备（备份盘 ↔ 实例盘）时 Rename 会返回 EXDEV
	if err := Copy(ctx, src, dst, overwrite, report); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// ---- 工具 ----

func copyFile(src string, dst io.Writer) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	buf := make([]byte, compressBufSize)
	return io.CopyBuffer(dst, in, buf)
}

func percent(done, total int64) int {
	if total <= 0 {
		return 0
	}
	p := int(done * 100 / total)
	if p > 99 {
		p = 99 // 收尾阶段（写目录项、rename）留 1% 给"完成"用
	}
	if p < 0 {
		p = 0
	}
	return p
}

func zipPercent(done, total int64) int {
	if total <= 0 {
		// 部分 zip 的头里 UncompressedSize64 为 0（流式写入产生），
		// 这时无法算百分比，只能显示一个"进行中"的进度。
		return 50
	}
	return percent(done, total)
}
