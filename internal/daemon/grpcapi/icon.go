package grpcapi

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 实例图标的文件名约定（按优先级）。
//
// server-icon.png 放前面是因为它是**原版/Paper 系的官方约定**（服务器列表里
// 显示的 64×64 图标就用这个名字，服务端自己不会生成，得用户自己放）；
// icon.png 则是一些整合包/面板的习惯叫法。两个都认，省得用户放对了名字却没生效。
var iconFileNames = []string{"server-icon.png", "icon.png"}

// maxIconBytes 图标大小上限。
//
// 图标是要**发给每个访问者**的（列表页每行一个），所以不能任其无限大：
// 超过这个尺寸就不认它 —— 否则一个误放的几百 MB 的 PNG 会变成每个用户
// 每次刷新列表都要下载的东西。超限只记一条日志说明原因。
const maxIconBytes = 2 << 20 // 2 MiB

// iconInfo 描述实例目录里的图标文件。
type iconInfo struct {
	Name  string
	Mtime int64 // Unix 秒
	Size  int64
}

// findIcon 查找实例目录里的图标（按 iconFileNames 的优先级）。
//
// 返回 ok=false 表示"没有可用图标"（不存在 / 不是普通文件 / 超限）——
// 调用方一律当成"没图标"处理，前端会回退到首字母头像。
func findIcon(dir string) (iconInfo, bool) {
	for _, name := range iconFileNames {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.Size() <= 0 {
			continue
		}
		if fi.Size() > maxIconBytes {
			slog.Warn("实例图标过大，已忽略",
				"dir", dir, "file", name, "size", fi.Size(), "limit", maxIconBytes)
			continue
		}
		return iconInfo{Name: name, Mtime: fi.ModTime().Unix(), Size: fi.Size()}, true
	}
	return iconInfo{}, false
}

// instanceIconMtime 供 GetInstanceStatus 上报图标修改时间（0 = 无图标）。
func instanceIconMtime(dir string) int64 {
	if ic, ok := findIcon(dir); ok {
		return ic.Mtime
	}
	return 0
}

// GetInstanceIcon 流式下发实例图标。
//
// 为什么单独一条 RPC 而不是复用 DownloadFile：文件名约定（两个候选、优先级、
// 大小上限）都该留在节点侧。若让面板传文件名，面板就得猜两次
// （"server-icon.png 失败再试 icon.png"），把节点上的约定泄漏到面板里。
func (s *Server) GetInstanceIcon(req *pb.InstanceRequest, stream pb.DaemonService_GetInstanceIconServer) error {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return err
	}
	ic, ok := findIcon(dir)
	if !ok {
		return errors.New("实例没有图标（可在实例目录放 server-icon.png 或 icon.png）")
	}

	f, err := os.Open(filepath.Join(dir, ic.Name))
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, downloadChunkSize)
	for {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			// 复制一份：buf 会被下一轮 Read 覆盖，而 gRPC 发送是异步的
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := stream.Send(&pb.FileChunk{Data: chunk, Total: ic.Size}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
