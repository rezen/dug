package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Row is one stored event. The reference is kept as JSON without its Raw
// upstream record, which can be large; the S3 inventory keeps the full one.
type Row struct {
	ID         uint      `gorm:"primaryKey"`
	Kind       string    `gorm:"index;not null"`
	OccurredAt time.Time `gorm:"index;not null"`
	Registry   string    `gorm:"index:idx_events_ref;not null"`
	Provider   string
	Catalog    string
	Namespace  string `gorm:"index:idx_events_ref"`
	Name       string `gorm:"index:idx_events_ref;not null"`
	Version    string `gorm:"index:idx_events_ref"`
	Key        string
	Error      string
	Reference  string `gorm:"not null"` // JSON registry.Reference minus Raw
}

func (Row) TableName() string { return "events" }

// SQLite stores events with gorm. Each batch is one transaction of multi-row
// inserts, so a flush costs one fsync however many events it holds.
type SQLite struct{ db *gorm.DB }

// OpenSQLite opens (creating if needed) the database at path and migrates the
// events table. It uses a pure-Go driver, so builds need no cgo.
func OpenSQLite(path string) (*SQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is required")
	}
	query := url.Values{"_pragma": {"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)"}}
	db, err := gorm.Open(sqlite.Open("file:"+path+"?"+query.Encode()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// One writer; extra connections would only contend on the database lock.
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&Row{}); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// DB exposes the handle for queries.
func (s *SQLite) DB() *gorm.DB { return s.db }

func (s *SQLite) Handle(ctx context.Context, batch []Event) error {
	rows := make([]Row, len(batch))
	for i, event := range batch {
		ref := event.Reference
		ref.Raw = nil
		body, err := json.Marshal(ref)
		if err != nil {
			return err
		}
		rows[i] = Row{Kind: string(event.Kind), OccurredAt: event.Time, Registry: ref.Registry, Provider: ref.Provider, Catalog: ref.Kind,
			Namespace: ref.Namespace, Name: ref.Name, Version: ref.Version, Key: event.Key, Error: event.Error, Reference: string(body)}
	}
	return s.db.WithContext(ctx).CreateInBatches(rows, 100).Error
}

func (s *SQLite) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
