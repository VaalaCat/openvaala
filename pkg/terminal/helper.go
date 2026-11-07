package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"time"
	"unicode/utf8"
)

// getPageEnd 计算输出分页的结束位置。
func getPageEnd(content []byte, maxBytes int) int {
	// 1. content 长于 maxBytes 时，从 maxBytes 向前最多查 UTFMax-1 字节，找到字符起点就截断；找不到或长度为零时按 maxBytes 切。
	for pageEnd := maxBytes; pageEnd > 0 && maxBytes-pageEnd < utf8.UTFMax; pageEnd-- {
		if utf8.RuneStart(content[pageEnd]) {
			return pageEnd
		}
	}
	return maxBytes
}

// completeUTF8Prefix 计算完整 UTF-8 前缀并报告解码状态。
func completeUTF8Prefix(content []byte) (complete int, incomplete bool, invalid bool) {
	// 1. 逐个解码字符，遇到末尾缺字节或首个非法字节时返回此前完整长度，分别标记 incomplete 或 invalid。
	for complete < len(content) {
		remaining := content[complete:]
		if !utf8.FullRune(remaining) {
			return complete, true, false
		}
		runeValue, size := utf8.DecodeRune(remaining)
		if runeValue == utf8.RuneError && size == 1 {
			return complete, false, true
		}
		complete += size
	}
	return complete, false, false
}

// newTerminalID 生成随机命令编号。
func newTerminalID() string {
	// 1. 读取 6 个随机字节，返回 cmd_ 加 12 位十六进制编号。
	random := make([]byte, 6)
	// 读取完整的随机字节，rand.Read 不返回错误。
	_, _ = rand.Read(random)
	return "cmd_" + hex.EncodeToString(random)
}

// newTerminalInfo 生成命令启动时的信息。
func newTerminalInfo(id string, request *TerminalRequest, startedAt time.Time) TerminalInfo {
	// 1. 复制编号和请求字段，状态设为 running。
	info := TerminalInfo{
		ID:            id,
		EnvironmentID: request.EnvironmentID,
		Command:       request.Command,
		Directory:     request.Directory,
		Interactive:   request.Interactive,
		StartedAt:     startedAt,
		Status:        TerminalStatusRunning,
	}
	// 2. 交互命令记录 PTY 初始行列数，为零时按 24×80。
	if request.Interactive {
		info.Rows, info.Cols = request.Rows, request.Cols
		if info.Rows == 0 {
			info.Rows = defaultTerminalRows
		}
		if info.Cols == 0 {
			info.Cols = defaultTerminalCols
		}
	}
	return info
}

// isChannelClosed 信号通道是否已关闭？
func isChannelClosed(channel <-chan struct{}) bool {
	// 1. 对只关闭、不发送数据的通道，非阻塞检查是否可接收关闭信号。
	select {
	case <-channel:
		return true
	default:
		return false
	}
}
