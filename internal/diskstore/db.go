package diskstore

import (
	"os"
	"sync"

	badger "github.com/dgraph-io/badger/v4"
)

// DB wraps a BadgerDB instance with lifecycle management.
type DB struct {
	db        *badger.DB
	dir       string
	tempDir   bool
	closeOnce sync.Once
	closeErr  error
}

// Open opens a BadgerDB at the given directory. If dir is "", a temporary
// directory is created and will be removed on Close.
func Open(dir string) (*DB, error) {
	temp := false
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "stumpt-badger-*")
		if err != nil {
			return nil, err
		}
		temp = true
	}

	opts := badger.DefaultOptions(dir).
		WithLogger(nil).
		WithNumVersionsToKeep(1).
		WithCompactL0OnClose(false).
		WithNumCompactors(2).
		WithValueLogFileSize(256 << 20).  // 256 MB value log files
		WithMemTableSize(64 << 20).       // 64 MB memtable
		WithNumMemtables(2).              // Keep 2 memtables
		WithBlockCacheSize(32 << 20).     // 32 MB block cache (required by compression)
		WithIndexCacheSize(100 << 20).    // 100 MB index cache
		WithDetectConflicts(false)        // No transaction conflicts expected

	bdb, err := badger.Open(opts)
	if err != nil {
		if temp {
			os.RemoveAll(dir)
		}
		return nil, err
	}

	return &DB{
		db:      bdb,
		dir:     dir,
		tempDir: temp,
	}, nil
}

// BadgerDB returns the underlying *badger.DB.
func (d *DB) BadgerDB() *badger.DB {
	return d.db
}

// Close closes the BadgerDB and removes the temp directory if applicable.
// Safe to call multiple times.
func (d *DB) Close() error {
	d.closeOnce.Do(func() {
		d.closeErr = d.db.Close()
		if d.tempDir {
			os.RemoveAll(d.dir)
		}
	})
	return d.closeErr
}
