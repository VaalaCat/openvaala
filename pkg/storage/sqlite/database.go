package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Database 是打开的 SQLite 库，提供建在同一个库上的记录存储和日志存储。
type Database interface {
	// Record 返回本数据库的记录存储。
	Record() storage.RecordStorage
	// Log 返回同一数据库中的日志存储。
	Log() storage.LogStorage
	// Close 关闭数据库连接，内存库的数据同时释放。
	Close() error
}

// databaseImpl 持有 SQLite 连接和建在它上面的记录存储、日志存储。
type databaseImpl struct {
	db      *sql.DB
	records *recordStorageImpl
	logs    *logStorageImpl
}

// DatabaseOpt 保存数据库位置、锁等待时间和要创建的记录分组。
type DatabaseOpt struct {
	// Path 是落盘库的文件路径；IsInMemory 为 true 时不使用。
	Path string
	// IsInMemory 库是否只放在内存里？内存库跟着这个 Database 的连接存在，Close 后释放。
	IsInMemory bool
	// BusyTimeout 是等待数据库锁的时间；为 0 时用 5 秒。
	BusyTimeout time.Duration
	// Namespaces 是打开时要创建的 Record 表；运行时访问未声明的 namespace 返回错误。
	Namespaces []storage.RecordNamespace
}

// NewDatabaseOpt 创建落盘 SQLite 配置。
func NewDatabaseOpt(path string, namespaces ...storage.RecordNamespace) *DatabaseOpt {
	// 1. 保存路径和记录分组，默认锁等待时间为 5 秒。
	return &DatabaseOpt{
		Path:        path,
		BusyTimeout: 5 * time.Second,
		Namespaces:  namespaces,
	}
}

// NewMemoryDatabaseOpt 创建仅保存在内存中的 SQLite 配置。
func NewMemoryDatabaseOpt(namespaces ...storage.RecordNamespace) *DatabaseOpt {
	// 1. 开启内存模式并保存记录分组，默认锁等待时间为 5 秒。
	return &DatabaseOpt{
		IsInMemory:  true,
		BusyTimeout: 5 * time.Second,
		Namespaces:  namespaces,
	}
}

// OpenDatabase 打开 SQLite 并创建记录表、索引和日志表。
func OpenDatabase(opt *DatabaseOpt) (Database, error) {
	// 1. 检查路径、等待时间及记录分组字段。
	if opt == nil || (!opt.IsInMemory && opt.Path == "") {
		return nil, errors.New("sqlite database path is required")
	}
	if opt.BusyTimeout < 0 {
		return nil, errors.New("sqlite busy timeout cannot be negative")
	}
	fieldPaths, err := getRecordFieldPaths(opt.Namespaces)
	if err != nil {
		return nil, err
	}
	// 2. 补齐等待时间并组装连接串；内存库使用随机名称，落盘库启用 WAL 和 FULL 同步。
	timeout := opt.BusyTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	busyTimeoutPragma := "&_pragma=busy_timeout(" + strconv.FormatInt(timeout.Milliseconds(), 10) + ")"
	var dsn string
	if opt.IsInMemory {
		dsn = "file:" + uuid.NewString() + "?mode=memory&_pragma=foreign_keys(1)" + busyTimeoutPragma
	} else {
		fileURL := (&url.URL{
			Scheme: "file",
			Path:   filepath.Clean(opt.Path),
		}).String()
		dsn = fileURL +
			"?_pragma=foreign_keys(1)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(FULL)" +
			busyTimeoutPragma
	}
	// 3. 打开并检查连接，连接数固定为 1；内存库在关闭这个连接后释放。
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("ping sqlite: %w", err), db.Close())
	}
	// 4. 为声明的记录分组创建表、(updated_at, key) 索引和字段联合索引；字段索引名是 <namespace>.<字段>.updated_at。
	tables := make(map[string]string, len(opt.Namespaces))
	for _, namespace := range opt.Namespaces {
		table := quoteIdentifier(namespace.Name)
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (
			key        TEXT COLLATE BINARY PRIMARY KEY,
			updated_at INTEGER NOT NULL,
			payload    BLOB NOT NULL
		)`); err != nil {
			return nil, errors.Join(fmt.Errorf("create record namespace %q: %w", namespace.Name, err), db.Close())
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS ` + quoteIdentifier(namespace.Name+"_updated_at") + ` ON ` + table + ` (updated_at, key)`); err != nil {
			return nil, errors.Join(fmt.Errorf("create record namespace %q index: %w", namespace.Name, err), db.Close())
		}
		for _, field := range namespace.IndexedFields {
			indexName := quoteIdentifier(namespace.Name + "." + field + ".updated_at")
			expression := "json_extract(CAST(payload AS TEXT), " + fieldPaths[namespace.Name][field] + ")"
			if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS ` + indexName + ` ON ` + table + ` (` + expression + `, updated_at, key)`); err != nil {
				return nil, errors.Join(fmt.Errorf("create record namespace %q field %q index: %w", namespace.Name, field, err), db.Close())
			}
		}
		tables[namespace.Name] = table
	}
	// 5. 创建日志表并返回数据库及两个存储实例，由调用方负责关闭；建表失败时关闭连接。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS logs (
		path       TEXT NOT NULL,
		stream_id  TEXT NOT NULL,
		ordinal    INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		payload    BLOB NOT NULL,
		PRIMARY KEY (path, stream_id, ordinal)
	)`); err != nil {
		return nil, errors.Join(fmt.Errorf("create log table: %w", err), db.Close())
	}
	return &databaseImpl{
		db: db,
		records: &recordStorageImpl{
			db:         db,
			tables:     tables,
			fieldPaths: fieldPaths,
		},
		logs: &logStorageImpl{db: db},
	}, nil
}

// Record 返回本数据库的记录存储。
func (database *databaseImpl) Record() storage.RecordStorage {
	// 1. 返回已创建的记录存储实例。
	return database.records
}

// Log 返回本数据库的日志存储。
func (database *databaseImpl) Log() storage.LogStorage {
	// 1. 返回已创建的日志存储实例。
	return database.logs
}

// Close 关闭 SQLite 连接。
func (database *databaseImpl) Close() error {
	// 1. 关闭底层连接并返回错误。
	return database.db.Close()
}
