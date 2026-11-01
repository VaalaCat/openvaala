package file

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
)

// logStorageImpl 把每条 Stream 存成根目录下的 <path>/<streamID>.jsonl 文件，每次读写各自打开和关闭文件。
type logStorageImpl struct {
	opt *LogStorageOpt
	// streamLogs 按 Stream 保存顺序号分配和行偏移。
	streamLogs syncmap.SyncMap[streamKey, *streamLog]
}

// streamKey 是一条 Stream 在根目录下的位置：Path 目录加 StreamID 文件名。
type streamKey struct {
	path     storage.StreamPath
	streamID storage.StreamID
}

// streamNamePattern 是 Path 与 StreamID 允许的字符；拒绝空串、`.`、`..` 和 `/`。
var streamNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LogStorageOpt 是文件日志存储的配置，创建后只读。
type LogStorageOpt struct {
	// RootDirectory 是保存日志文件的根目录，必须是绝对路径。
	RootDirectory string
}

// NewLogStorageOpt 创建以指定目录保存日志文件的配置。
func NewLogStorageOpt(rootDirectory string) *LogStorageOpt {
	// 1. 保存根目录。
	return &LogStorageOpt{RootDirectory: rootDirectory}
}

// NewLogStorage 创建文件日志存储；同一根目录只能由一个进程写入。
func NewLogStorage(opt *LogStorageOpt) (storage.LogStorage, error) {
	// 1. 要求根目录是绝对路径，清理路径后保存。
	if opt == nil || !filepath.IsAbs(opt.RootDirectory) {
		return nil, errors.New("file log requires an absolute root directory")
	}
	return &logStorageImpl{
		opt: &LogStorageOpt{RootDirectory: filepath.Clean(opt.RootDirectory)},
	}, nil
}

var _ storage.LogStorage = (*logStorageImpl)(nil)

// logLine 是 JSONL 文件里的一行：顺序号、追加时间和载荷。
type logLine struct {
	Ordinal   storage.Ordinal `json:"ordinal"`
	UpdatedAt int64           `json:"updated_at"`
	Payload   json.RawMessage `json:"payload"`
}

// streamLog 串行维护一条 Stream 的顺序号分配和文件行索引。
type streamLog struct {
	// mutex 串行执行这条 Stream 的读写和删除。
	mutex sync.Mutex
	// nextOrdinal 是下一次追加分到的顺序号。
	nextOrdinal storage.Ordinal
	// offsets 是每一行在文件里的起始字节位置，下标是顺序号减 1。
	offsets []int64
	// initialized 行索引是否已经从文件读出？写入或关闭失败后重置为 false，下次读写重新扫描文件。
	initialized bool
}

// Append 向指定 Stream 追加一批日志。
func (logs *logStorageImpl) Append(
	ctx context.Context,
	path storage.StreamPath,
	streamID storage.StreamID,
	payloads ...json.RawMessage,
) ([]storage.LogEntryInfo, error) {
	// 1. 检查取消、路径和非空 JSON 载荷。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := streamKey{
		path:     path,
		streamID: streamID,
	}
	filePath, err := logs.streamFilePath(key)
	if err != nil {
		return nil, err
	}
	if len(payloads) == 0 {
		return nil, errors.New("log append requires at least one payload")
	}
	for index, payload := range payloads {
		if !json.Valid(payload) {
			return nil, fmt.Errorf("log payload %d is not valid JSON", index)
		}
	}
	// 2. 锁住这条 Stream，初始化行索引后追加本批日志。
	logStream, _ := logs.streamLogs.LoadOrStore(key, &streamLog{})
	logStream.mutex.Lock()
	defer logStream.mutex.Unlock()
	if err := logs.initializeStream(logStream, filePath); err != nil {
		return nil, err
	}
	return logs.appendLines(ctx, logStream, filePath, payloads)
}

// Delete 删除一条 Stream。
func (logs *logStorageImpl) Delete(ctx context.Context, path storage.StreamPath, streamID storage.StreamID) error {
	// 1. 检查取消和路径，锁住这条 Stream。
	if err := ctx.Err(); err != nil {
		return err
	}
	key := streamKey{
		path:     path,
		streamID: streamID,
	}
	filePath, err := logs.streamFilePath(key)
	if err != nil {
		return err
	}
	logStream, _ := logs.streamLogs.LoadOrStore(key, &streamLog{})
	logStream.mutex.Lock()
	defer logStream.mutex.Unlock()

	// 2. 删除文件，不存在时继续；缓存顺序号重置为 1。
	removed := false
	if err := os.Remove(filePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete log stream: %w", err)
		}
	} else {
		removed = true
	}
	logStream.nextOrdinal = 1
	logStream.offsets = []int64{}
	logStream.initialized = true
	// 3. 文件已删除时同步所属目录。
	if removed {
		if err := syncDirectory(filepath.Dir(filePath)); err != nil {
			return err
		}
	}
	return nil
}

// DeletePath 删除路径目录及其全部 Stream 文件和缓存。
func (logs *logStorageImpl) DeletePath(ctx context.Context, path storage.StreamPath) error {
	// 1. 检查取消和路径名称。
	if err := ctx.Err(); err != nil {
		return err
	}
	if !streamNamePattern.MatchString(string(path)) {
		return fmt.Errorf("invalid log path %q", path)
	}
	// 2. 删除目录并移除这个路径下各 Stream 的缓存，之后追加从顺序号 1 开始。
	directory := filepath.Join(logs.opt.RootDirectory, string(path))
	_, statErr := os.Lstat(directory)
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("delete log path: %w", err)
	}
	logs.streamLogs.Range(func(key streamKey, _ *streamLog) bool {
		if key.path == path {
			logs.streamLogs.Delete(key)
		}
		return true
	})
	// 3. 目录原本不存在时直接成功，否则同步根目录。
	if errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	return syncDirectory(logs.opt.RootDirectory)
}

// appendLines 将一批载荷编码成连续 JSONL 行并写入文件。
func (logs *logStorageImpl) appendLines(
	ctx context.Context,
	logStream *streamLog,
	filePath string,
	payloads []json.RawMessage,
) (infos []storage.LogEntryInfo, err error) {
	// 1. 检查取消，为本批分配连续顺序号、同一时间及各行相对偏移。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	infos = make([]storage.LogEntryInfo, len(payloads))
	lineOffsets := make([]int64, len(payloads))
	var content []byte
	updatedAt := time.Now().UnixMilli()
	for index, payload := range payloads {
		ordinal := logStream.nextOrdinal + storage.Ordinal(index)
		infos[index] = storage.LogEntryInfo{
			Ordinal:   ordinal,
			UpdatedAt: updatedAt,
		}
		line, err := json.Marshal(logLine{
			Ordinal:   ordinal,
			UpdatedAt: updatedAt,
			Payload:   payload,
		})
		if err != nil {
			return nil, fmt.Errorf("encode log line: %w", err)
		}
		lineOffsets[index] = int64(len(content))
		content = append(append(content, line...), '\n')
	}

	// 2. 检查文件和目录是否新建，创建目录并打开追加文件。
	directory := filepath.Dir(filePath)
	_, statErr := os.Stat(filePath)
	newFile := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !newFile {
		return nil, fmt.Errorf("inspect log stream: %w", statErr)
	}
	_, directoryErr := os.Stat(directory)
	newDirectory := errors.Is(directoryErr, os.ErrNotExist)
	if directoryErr != nil && !newDirectory {
		return nil, fmt.Errorf("inspect log directory: %w", directoryErr)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	streamFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open log stream: %w", err)
	}
	defer func() {
		if closeErr := streamFile.Close(); closeErr != nil {
			logStream.initialized = false
			err = errors.Join(err, fmt.Errorf("close log stream: %w", closeErr))
		}
	}()
	streamInfo, err := streamFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened log stream: %w", err)
	}
	// 3. 记下写入前的文件长度；之后写入或同步失败时把行索引标记为需要重新扫描，并把文件截回这个长度。
	// 关闭失败只标记，不截断。
	fileOffset := streamInfo.Size()
	rollbackBatch := func(cause error) error {
		logStream.initialized = false
		if truncateErr := streamFile.Truncate(fileOffset); truncateErr != nil {
			return errors.Join(cause, fmt.Errorf("truncate log stream: %w", truncateErr))
		}
		return cause
	}
	// 4. 完整写入本批并同步文件，新文件或目录另行同步对应目录。
	if err := writeAll(streamFile, content); err != nil {
		return nil, rollbackBatch(fmt.Errorf("write log lines: %w", err))
	}
	if err := streamFile.Sync(); err != nil {
		return nil, rollbackBatch(fmt.Errorf("sync log stream: %w", err))
	}
	if newFile {
		if err := syncDirectory(directory); err != nil {
			return nil, rollbackBatch(err)
		}
	}
	if newDirectory {
		if err := syncDirectory(logs.opt.RootDirectory); err != nil {
			return nil, rollbackBatch(err)
		}
	}

	// 5. 写入成功后保存各行偏移并推进下一顺序号。
	for _, lineOffset := range lineOffsets {
		logStream.offsets = append(logStream.offsets, fileOffset+lineOffset)
	}
	logStream.nextOrdinal += storage.Ordinal(len(payloads))
	return infos, nil
}

// List 按顺序号分页读取指定 Stream。
func (logs *logStorageImpl) List(
	ctx context.Context,
	request storage.ListLogEntriesRequest,
) (page storage.Page[storage.LogEntry, storage.Ordinal], err error) {
	// 1. 检查取消、路径、页大小和方向。
	if err := ctx.Err(); err != nil {
		return page, err
	}
	key := streamKey{
		path:     request.Path,
		streamID: request.StreamID,
	}
	filePath, err := logs.streamFilePath(key)
	if err != nil {
		return page, err
	}
	if request.Limit <= 0 {
		return page, errors.New("log entry limit must be positive")
	}
	order, err := request.Order.WithDefault()
	if err != nil {
		return page, err
	}
	request.Order = order

	// 2. 锁住 Stream 并初始化索引，检查游标是否超过日志末尾。
	logStream, _ := logs.streamLogs.LoadOrStore(key, &streamLog{})
	logStream.mutex.Lock()
	defer logStream.mutex.Unlock()
	if err := logs.initializeStream(logStream, filePath); err != nil {
		return page, err
	}
	count := len(logStream.offsets)
	if request.Cursor > storage.Ordinal(count) {
		return page, fmt.Errorf("ordinal %d is beyond log tail %d", request.Cursor, count)
	}
	// 3. 选择本页顺序号，无记录时返回空页。
	ordinals := selectOrdinals(count, request)
	if len(ordinals) == 0 {
		return storage.Page[storage.LogEntry, storage.Ordinal]{Items: []storage.LogEntry{}}, nil
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	// 4. 打开文件并按偏移读取日志，结束时关闭文件。
	streamFile, err := os.Open(filePath)
	if err != nil {
		return page, fmt.Errorf("open log stream: %w", err)
	}
	defer func() {
		if closeErr := streamFile.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close log stream: %w", closeErr))
		}
	}()
	streamInfo, err := streamFile.Stat()
	if err != nil {
		return page, fmt.Errorf("inspect opened log stream: %w", err)
	}
	entries, err := readLogEntries(ctx, streamFile, streamInfo.Size(), logStream.offsets, ordinals)
	if err != nil {
		return page, err
	}
	// 5. 返回本页，有更多时使用末项顺序号作为下一页游标。
	page.Items = entries
	if isMoreLogEntries(count, request.Order, ordinals[len(ordinals)-1]) {
		page.Next = ordinals[len(ordinals)-1]
	}
	return page, nil
}

// GetStreamPaths 返回含有指定 Stream 文件的路径。
func (logs *logStorageImpl) GetStreamPaths(ctx context.Context, streamID storage.StreamID) ([]storage.StreamPath, error) {
	// 1. 检查取消和 Stream 名称，根目录不存在时返回空列表。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !streamNamePattern.MatchString(string(streamID)) {
		return nil, fmt.Errorf("invalid log stream ID %q", streamID)
	}
	entries, err := os.ReadDir(logs.opt.RootDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return []storage.StreamPath{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read log root: %w", err)
	}
	// 2. 按名称升序逐个检查有效子目录中的普通日志文件，收集匹配路径。
	paths := []storage.StreamPath{}
	for _, entry := range entries {
		if !entry.IsDir() || !streamNamePattern.MatchString(entry.Name()) {
			continue
		}
		info, err := os.Lstat(filepath.Join(logs.opt.RootDirectory, entry.Name(), string(streamID)+".jsonl"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect log stream: %w", err)
		}
		if info.Mode().IsRegular() {
			paths = append(paths, storage.StreamPath(entry.Name()))
		}
	}
	return paths, nil
}

// initializeStream 还没读过行索引时扫描日志文件，建立各行偏移和下一顺序号。
func (logs *logStorageImpl) initializeStream(logStream *streamLog, filePath string) error {
	// 1. 已初始化时直接返回，否则打开文件；文件不存在时创建空索引。
	if logStream.initialized {
		return nil
	}
	streamFile, err := os.OpenFile(filePath, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		logStream.nextOrdinal = 1
		logStream.offsets = []int64{}
		logStream.initialized = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("open log stream: %w", err)
	}
	// 2. 扫描日志并截去未写完的尾行，再关闭文件，合并扫描和关闭错误。
	offsets, scanErr := scanAndRepairStream(streamFile)
	closeErr := streamFile.Close()
	if scanErr != nil {
		if closeErr != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("close log stream: %w", closeErr))
		}
		return scanErr
	}
	if closeErr != nil {
		return fmt.Errorf("close log stream: %w", closeErr)
	}
	// 3. 保存行偏移和下一顺序号，标记初始化完成。
	logStream.nextOrdinal = storage.Ordinal(len(offsets)) + 1
	logStream.offsets = offsets
	logStream.initialized = true
	return nil
}

// scanAndRepairStream 扫描修复 JSONL 日志，返回完整行偏移。
func scanAndRepairStream(streamFile *os.File) ([]int64, error) {
	// 1. 将文件移至起点并逐行读取。
	if _, err := streamFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek log stream: %w", err)
	}
	reader := bufio.NewReader(streamFile)
	offsets := make([]int64, 0, 64)
	var completeOffset int64
	for {
		line, err := reader.ReadBytes('\n')
		// 2. 检查完整行的 JSON 载荷和连续顺序号，保存行偏移；完整行损坏时返回错误。
		if len(line) > 0 && line[len(line)-1] == '\n' {
			expectedOrdinal := storage.Ordinal(len(offsets)) + 1
			var persisted logLine
			if len(line) == 1 || json.Unmarshal(line[:len(line)-1], &persisted) != nil || len(persisted.Payload) == 0 || !json.Valid(persisted.Payload) {
				return nil, fmt.Errorf("invalid JSONL item at ordinal %d", expectedOrdinal)
			}
			if persisted.Ordinal != expectedOrdinal {
				return nil, fmt.Errorf("log ordinal %d at line %d, want %d", persisted.Ordinal, expectedOrdinal, expectedOrdinal)
			}
			offsets = append(offsets, completeOffset)
			completeOffset += int64(len(line))
		}
		// 3. 到达文件尾时截去没有换行的尾行并同步，返回各行偏移；其他读取错误直接返回。
		if errors.Is(err, io.EOF) {
			if len(line) > 0 {
				if err := streamFile.Truncate(completeOffset); err != nil {
					return nil, fmt.Errorf("truncate incomplete log tail: %w", err)
				}
				if err := streamFile.Sync(); err != nil {
					return nil, fmt.Errorf("sync repaired log tail: %w", err)
				}
			}
			return offsets, nil
		}
		if err != nil {
			return nil, fmt.Errorf("inspect log stream: %w", err)
		}
	}
}

// selectOrdinals 选择本页要读取的日志顺序号。
func selectOrdinals(count int, request storage.ListLogEntriesRequest) []storage.Ordinal {
	// 1. 空日志返回空列表，正序从游标后向末尾取指定数量。
	if count == 0 {
		return []storage.Ordinal{}
	}
	if request.Order == storage.DirectionAsc {
		start := int(request.Cursor)
		selectedCount := min(request.Limit, count-start)
		ordinals := make([]storage.Ordinal, selectedCount)
		for index := range ordinals {
			ordinals[index] = storage.Ordinal(start + index + 1)
		}
		return ordinals
	}

	// 2. 倒序从末尾或游标前一项向前取指定数量。
	end := count
	if request.Cursor > 0 {
		end = int(request.Cursor) - 1
	}
	selectedCount := min(request.Limit, end)
	ordinals := make([]storage.Ordinal, selectedCount)
	for index := range ordinals {
		ordinals[index] = storage.Ordinal(end - index)
	}
	return ordinals
}

// isMoreLogEntries 本页末项之后，指定方向还有日志吗？
func isMoreLogEntries(count int, order storage.Direction, lastOrdinal storage.Ordinal) bool {
	// 1. 正序比较日志末尾，倒序比较顺序号 1。
	if order == storage.DirectionAsc {
		return lastOrdinal < storage.Ordinal(count)
	}
	return lastOrdinal > 1
}

// readLogEntries 按给定顺序号和行偏移读取日志。
func readLogEntries(
	ctx context.Context,
	streamFile *os.File,
	fileSize int64,
	offsets []int64,
	ordinals []storage.Ordinal,
) ([]storage.LogEntry, error) {
	// 1. 逐项检查取消，并从行偏移及文件长度计算读取范围。
	entries := make([]storage.LogEntry, 0, len(ordinals))
	for _, ordinal := range ordinals {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index := int(ordinal - 1)
		start := offsets[index]
		end := fileSize
		if index+1 < len(offsets) {
			end = offsets[index+1]
		}
		// 2. 读取整行，检查换行、JSON 载荷和顺序号，再收集日志内容。
		line := make([]byte, int(end-start))
		if _, err := streamFile.ReadAt(line, start); err != nil {
			return nil, fmt.Errorf("read log ordinal %d: %w", ordinal, err)
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return nil, fmt.Errorf("log ordinal %d has no complete line", ordinal)
		}
		var persisted logLine
		if err := json.Unmarshal(line[:len(line)-1], &persisted); err != nil {
			return nil, fmt.Errorf("decode log ordinal %d: %w", ordinal, err)
		}
		if persisted.Ordinal != ordinal || len(persisted.Payload) == 0 || !json.Valid(persisted.Payload) {
			return nil, fmt.Errorf("invalid log ordinal %d", ordinal)
		}
		entries = append(entries, storage.LogEntry{
			Ordinal:   ordinal,
			UpdatedAt: persisted.UpdatedAt,
			Payload:   persisted.Payload,
		})
	}
	return entries, nil
}

// streamFilePath 检查日志位置并返回对应的 JSONL 文件路径。
func (logs *logStorageImpl) streamFilePath(key streamKey) (string, error) {
	// 1. 检查路径和 Stream 名称，再拼接 root/path/streamID.jsonl。
	if !streamNamePattern.MatchString(string(key.path)) || !streamNamePattern.MatchString(string(key.streamID)) {
		return "", fmt.Errorf("invalid log stream %q/%q", key.path, key.streamID)
	}
	return filepath.Join(logs.opt.RootDirectory, string(key.path), string(key.streamID)+".jsonl"), nil
}

// syncDirectory 打开目录执行 fsync 后关闭。
func syncDirectory(path string) error {
	// 1. 打开目录。
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log directory: %w", err)
	}
	// 2. 同步后关闭目录，将两次操作的错误合并返回。
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		err = errors.Join(err, fmt.Errorf("sync log directory: %w", syncErr))
	}
	if closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close log directory: %w", closeErr))
	}
	return err
}

// writeAll 将全部内容写入指定 Writer。
func writeAll(writer io.Writer, content []byte) error {
	// 1. 循环写入剩余字节，遇到错误或零字节写入时返回错误。
	for len(content) > 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}
