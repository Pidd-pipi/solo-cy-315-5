package database

import (
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// SQLiteDSN appends concurrency-friendly pragmas to a SQLite DSN.
//
//   - busy_timeout makes writers wait for an in-flight lock instead of failing
//     immediately with SQLITE_BUSY under concurrent publishes/rollbacks.
//   - WAL lets readers and a single writer coexist on file-backed databases.
//   - foreign_keys enforces FK constraints per connection.
func SQLiteDSN(path string) string {
	return dsnWithPragmas(path, "WAL")
}

// SQLiteMemoryDSN is the in-memory counterpart of SQLiteDSN. In-memory databases
// do not support WAL journal mode, so it uses the default rollback journal
// while keeping busy_timeout and foreign_keys enabled.
func SQLiteMemoryDSN(path string) string {
	return dsnWithPragmas(path, "MEMORY")
}

func dsnWithPragmas(path, journalMode string) string {
	pragmas := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(" + journalMode + ")",
		"_pragma=foreign_keys(ON)",
	}
	if strings.Contains(path, "?") {
		return path + "&" + strings.Join(pragmas, "&")
	}
	return path + "?" + strings.Join(pragmas, "&")
}

// Open opens a GORM SQLite database with the standard pragmas.
func Open(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(SQLiteDSN(path)), &gorm.Config{
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access underlying db: %w", err)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetConnMaxLifetime(time.Hour)
	return db, nil
}

// OpenMemory opens a GORM SQLite in-memory database suitable for tests; it
// accepts a caller-provided DSN (typically file:<name>?mode=memory&cache=shared).
func OpenMemory(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(SQLiteMemoryDSN(dsn)), &gorm.Config{
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite memory: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("access underlying db: %w", err)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetConnMaxLifetime(time.Hour)
	return db, nil
}
