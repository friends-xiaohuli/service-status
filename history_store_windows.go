//go:build windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sqliteOK                           = 0
	sqliteRow                          = 100
	sqliteDone                         = 101
	persistenceCleanupInterval         = 6 * time.Hour
	sqliteTransient            uintptr = ^uintptr(0)
)

var (
	winsqlite3DLL         = syscall.NewLazyDLL("winsqlite3.dll")
	kernel32DLL           = syscall.NewLazyDLL("kernel32.dll")
	procSQLiteOpen16      = winsqlite3DLL.NewProc("sqlite3_open16")
	procSQLiteCloseV2     = winsqlite3DLL.NewProc("sqlite3_close_v2")
	procSQLiteErrMsg      = winsqlite3DLL.NewProc("sqlite3_errmsg")
	procSQLiteExec        = winsqlite3DLL.NewProc("sqlite3_exec")
	procSQLitePrepareV2   = winsqlite3DLL.NewProc("sqlite3_prepare_v2")
	procSQLiteStep        = winsqlite3DLL.NewProc("sqlite3_step")
	procSQLiteFinalize    = winsqlite3DLL.NewProc("sqlite3_finalize")
	procSQLiteBindText    = winsqlite3DLL.NewProc("sqlite3_bind_text")
	procSQLiteBindInt64   = winsqlite3DLL.NewProc("sqlite3_bind_int64")
	procSQLiteColumnText  = winsqlite3DLL.NewProc("sqlite3_column_text")
	procSQLiteColumnInt64 = winsqlite3DLL.NewProc("sqlite3_column_int64")
	procLstrlenA          = kernel32DLL.NewProc("lstrlenA")
	procRtlMoveMemory     = kernel32DLL.NewProc("RtlMoveMemory")
)

type historyStore struct {
	mu          sync.Mutex
	db          uintptr
	lastCleanup time.Time
}

type sqliteStmt struct {
	store  *historyStore
	handle uintptr
}

func openHistoryStore(path string) (*historyStore, error) {
	if err := winsqlite3DLL.Load(); err != nil {
		return nil, fmt.Errorf("加载 winsqlite3.dll 失败: %w", err)
	}

	utf16Path, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("数据库路径无效: %w", err)
	}

	var db uintptr
	rc, _, _ := procSQLiteOpen16.Call(
		uintptr(unsafe.Pointer(utf16Path)),
		uintptr(unsafe.Pointer(&db)),
	)
	store := &historyStore{db: db}
	if int(rc) != sqliteOK {
		msg := store.errMsg()
		_ = store.closeUnlocked()
		if strings.TrimSpace(msg) == "" {
			msg = fmt.Sprintf("SQLite 打开失败 (rc=%d)", rc)
		}
		return nil, errors.New(msg)
	}

	if err := store.execUnlocked(`PRAGMA busy_timeout = 5000;`); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.execUnlocked(`PRAGMA synchronous = NORMAL;`); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.execUnlocked(`
		CREATE TABLE IF NOT EXISTS service_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			service_key TEXT NOT NULL,
			service_name TEXT NOT NULL,
			service_group TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			response_time INTEGER NOT NULL,
			display_time TEXT NOT NULL,
			ts INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_service_history_key_ts ON service_history(service_key, ts);
		CREATE INDEX IF NOT EXISTS idx_service_history_ts ON service_history(ts);
	`); err != nil {
		_ = store.Close()
		return nil, err
	}

	return store, nil
}

func (s *historyStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeUnlocked()
}

func (s *historyStore) closeUnlocked() error {
	if s.db == 0 {
		return nil
	}
	rc, _, _ := procSQLiteCloseV2.Call(s.db)
	if int(rc) != sqliteOK {
		return s.sqliteError(int(rc))
	}
	s.db = 0
	return nil
}

func (s *historyStore) Insert(key string, cfg ServiceConfig, entry HistoryEntry) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stmt, err := s.prepareUnlocked(`
		INSERT INTO service_history (
			service_key,
			service_name,
			service_group,
			status,
			response_time,
			display_time,
			ts
		) VALUES (?, ?, ?, ?, ?, ?, ?);
	`)
	if err != nil {
		return err
	}
	defer stmt.Finalize()

	if err := stmt.BindText(1, key); err != nil {
		return err
	}
	if err := stmt.BindText(2, cfg.Name); err != nil {
		return err
	}
	if err := stmt.BindText(3, cfg.Group); err != nil {
		return err
	}
	if err := stmt.BindText(4, entry.Status); err != nil {
		return err
	}
	if err := stmt.BindInt64(5, entry.ResponseTime); err != nil {
		return err
	}
	if err := stmt.BindText(6, formatStorageDisplayTime(time.Unix(entry.Ts, 0))); err != nil {
		return err
	}
	if err := stmt.BindInt64(7, entry.Ts); err != nil {
		return err
	}

	rc, err := stmt.Step()
	if err != nil {
		return err
	}
	if rc != sqliteDone {
		return fmt.Errorf("SQLite 插入未完成 (rc=%d)", rc)
	}
	return nil
}

func (s *historyStore) LoadWindow(keys []string, since time.Time) (map[string][]HistoryEntry, error) {
	result := make(map[string][]HistoryEntry)
	if s == nil {
		return result, nil
	}

	keys = dedupeStrings(keys)
	if len(keys) == 0 {
		return result, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	query := fmt.Sprintf(`
		SELECT
			service_key,
			status,
			response_time,
			display_time,
			ts
		FROM service_history
		WHERE ts >= ? AND service_key IN (%s)
		ORDER BY service_key ASC, ts ASC;
	`, placeholders)

	stmt, err := s.prepareUnlocked(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()

	if err := stmt.BindInt64(1, since.Unix()); err != nil {
		return nil, err
	}
	for i, key := range keys {
		if err := stmt.BindText(i+2, key); err != nil {
			return nil, err
		}
	}

	for {
		rc, err := stmt.Step()
		if err != nil {
			return nil, err
		}
		if rc == sqliteDone {
			break
		}
		if rc != sqliteRow {
			return nil, fmt.Errorf("SQLite 读取历史失败 (rc=%d)", rc)
		}

		serviceKey := stmt.ColumnText(0)
		ts := stmt.ColumnInt64(4)
		result[serviceKey] = append(result[serviceKey], HistoryEntry{
			Status:       stmt.ColumnText(1),
			ResponseTime: stmt.ColumnInt64(2),
			Time:         formatHistoryTime(time.Unix(ts, 0)),
			Ts:           ts,
		})
	}

	return result, nil
}

func (s *historyStore) MaybeCleanup(now time.Time) error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.lastCleanup.IsZero() && now.Sub(s.lastCleanup) < persistenceCleanupInterval {
		return nil
	}

	stmt, err := s.prepareUnlocked(`DELETE FROM service_history WHERE ts < ?;`)
	if err != nil {
		return err
	}
	defer stmt.Finalize()

	if err := stmt.BindInt64(1, now.Add(-persistentHistoryWindow).Unix()); err != nil {
		return err
	}

	rc, err := stmt.Step()
	if err != nil {
		return err
	}
	if rc != sqliteDone {
		return fmt.Errorf("SQLite 清理未完成 (rc=%d)", rc)
	}

	s.lastCleanup = now
	return nil
}

func (s *historyStore) prepareUnlocked(sql string) (*sqliteStmt, error) {
	if s.db == 0 {
		return nil, errors.New("SQLite 连接尚未打开")
	}

	sqlPtr, err := utf8CStringPtr(sql)
	if err != nil {
		return nil, err
	}
	var stmt uintptr
	rc, _, _ := procSQLitePrepareV2.Call(
		s.db,
		uintptr(unsafe.Pointer(sqlPtr)),
		^uintptr(0),
		uintptr(unsafe.Pointer(&stmt)),
		0,
	)
	runtime.KeepAlive(sqlPtr)
	if int(rc) != sqliteOK {
		return nil, s.sqliteError(int(rc))
	}
	return &sqliteStmt{store: s, handle: stmt}, nil
}

func (s *historyStore) execUnlocked(sql string) error {
	if s.db == 0 {
		return errors.New("SQLite 连接尚未打开")
	}

	sqlPtr, err := utf8CStringPtr(sql)
	if err != nil {
		return err
	}
	rc, _, _ := procSQLiteExec.Call(s.db, uintptr(unsafe.Pointer(sqlPtr)), 0, 0, 0)
	runtime.KeepAlive(sqlPtr)
	if int(rc) != sqliteOK {
		return s.sqliteError(int(rc))
	}
	return nil
}

func (s *historyStore) sqliteError(rc int) error {
	msg := s.errMsg()
	if strings.TrimSpace(msg) == "" {
		msg = fmt.Sprintf("SQLite 返回错误码 %d", rc)
	}
	return errors.New(msg)
}

func (s *historyStore) errMsg() string {
	if s == nil || s.db == 0 {
		return ""
	}
	ptr, _, _ := procSQLiteErrMsg.Call(s.db)
	return utf8FromRawPtr(ptr)
}

func (s *sqliteStmt) Finalize() error {
	if s == nil || s.handle == 0 {
		return nil
	}
	rc, _, _ := procSQLiteFinalize.Call(s.handle)
	if int(rc) != sqliteOK {
		return s.store.sqliteError(int(rc))
	}
	s.handle = 0
	return nil
}

func (s *sqliteStmt) BindText(index int, value string) error {
	textPtr, err := utf8CStringPtr(value)
	if err != nil {
		return err
	}
	rc, _, _ := procSQLiteBindText.Call(
		s.handle,
		uintptr(index),
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(len(value)),
		sqliteTransient,
	)
	runtime.KeepAlive(textPtr)
	if int(rc) != sqliteOK {
		return s.store.sqliteError(int(rc))
	}
	return nil
}

func (s *sqliteStmt) BindInt64(index int, value int64) error {
	rc, _, _ := procSQLiteBindInt64.Call(s.handle, uintptr(index), uintptr(value))
	if int(rc) != sqliteOK {
		return s.store.sqliteError(int(rc))
	}
	return nil
}

func (s *sqliteStmt) Step() (int, error) {
	rc, _, _ := procSQLiteStep.Call(s.handle)
	if int(rc) == sqliteRow || int(rc) == sqliteDone {
		return int(rc), nil
	}
	return int(rc), s.store.sqliteError(int(rc))
}

func (s *sqliteStmt) ColumnText(index int) string {
	ptr, _, _ := procSQLiteColumnText.Call(s.handle, uintptr(index))
	return utf8FromRawPtr(ptr)
}

func (s *sqliteStmt) ColumnInt64(index int) int64 {
	value, _, _ := procSQLiteColumnInt64.Call(s.handle, uintptr(index))
	return int64(value)
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func utf8CStringPtr(value string) (*byte, error) {
	ptr, err := syscall.BytePtrFromString(value)
	if err != nil {
		return nil, fmt.Errorf("字符串包含无效的 NUL 字符: %w", err)
	}
	return ptr, nil
}

func utf8FromRawPtr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	size, _, _ := procLstrlenA.Call(ptr)
	if size == 0 {
		return ""
	}
	buf := make([]byte, int(size))
	procRtlMoveMemory.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		ptr,
		size,
	)
	return string(buf)
}
