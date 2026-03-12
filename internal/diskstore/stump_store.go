package diskstore

import (
	"encoding/binary"
	"log/slog"
	"sync/atomic"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/bsv-blockchain/stumpt/internal/stump"
)

const (
	// stumpPrefix is the single-byte namespace prefix for stump entries.
	stumpPrefix byte = 's'
	// stumpKeySize is prefix (1) + XOR key (32) = 33 bytes (before sequence suffix).
	stumpKeySize = 1 + 32
	// fullKeySize is prefix (1) + XOR key (32) + sequence (8) = 41 bytes.
	fullKeySize = stumpKeySize + 8
)

// DiskStumpStore implements stump.StumpStore backed by BadgerDB.
// Each entry is stored under its own BadgerDB key:
//
//	's' (1 byte) | XOR key (32 bytes) | sequence number (8 bytes big-endian)
type DiskStumpStore struct {
	db  *DB
	seq atomic.Uint64
}

// NewDiskStumpStore creates a new DiskStumpStore backed by the given DB.
func NewDiskStumpStore(db *DB) *DiskStumpStore {
	return &DiskStumpStore{db: db}
}

// makeDBKey builds the full BadgerDB key for a stump entry.
func makeDBKey(key stump.Key, seq uint64) []byte {
	buf := make([]byte, fullKeySize)
	buf[0] = stumpPrefix
	copy(buf[1:], key[:])
	binary.BigEndian.PutUint64(buf[stumpKeySize:], seq)
	return buf
}

// makePrefixKey builds the 33-byte prefix for scanning all entries under an XOR key.
func makePrefixKey(key stump.Key) []byte {
	buf := make([]byte, stumpKeySize)
	buf[0] = stumpPrefix
	copy(buf[1:], key[:])
	return buf
}

// Append adds a single entry under the given XOR key.
func (s *DiskStumpStore) Append(key stump.Key, e *stump.Entry) {
	seq := s.seq.Add(1)
	dbKey := makeDBKey(key, seq)
	val := MarshalEntry(e)

	err := s.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(dbKey, val)
	})
	if err != nil {
		slog.Error("DiskStumpStore.Append failed", "error", err)
	}
}

// AppendBatch adds multiple entries under the given XOR key using a WriteBatch.
func (s *DiskStumpStore) AppendBatch(key stump.Key, entries []*stump.Entry) {
	if len(entries) == 0 {
		return
	}

	wb := s.db.BadgerDB().NewWriteBatch()
	for _, e := range entries {
		seq := s.seq.Add(1)
		dbKey := makeDBKey(key, seq)
		val := MarshalEntry(e)
		if err := wb.Set(dbKey, val); err != nil {
			slog.Error("DiskStumpStore.AppendBatch set failed", "error", err)
			wb.Cancel()
			return
		}
	}

	if err := wb.Flush(); err != nil {
		slog.Error("DiskStumpStore.AppendBatch flush failed", "error", err)
	}
}

// Get returns all entries stored under the given XOR key.
// Returns nil if no entries exist.
func (s *DiskStumpStore) Get(key stump.Key) []*stump.Entry {
	prefix := makePrefixKey(key)
	var result []*stump.Entry

	err := s.db.BadgerDB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			err := item.Value(func(val []byte) error {
				e, err := UnmarshalEntry(val)
				if err != nil {
					slog.Error("DiskStumpStore.Get unmarshal failed", "error", err)
					return nil // skip corrupt entries
				}
				result = append(result, e)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("DiskStumpStore.Get failed", "error", err)
	}

	return result
}

// Len returns the number of unique XOR keys in the store.
// This is an expensive operation that performs a full prefix scan; use only for diagnostics.
func (s *DiskStumpStore) Len() int {
	seen := make(map[[32]byte]struct{})
	prefix := []byte{stumpPrefix}

	err := s.db.BadgerDB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			k := it.Item().Key()
			if len(k) < stumpKeySize {
				continue
			}
			var xorKey [32]byte
			copy(xorKey[:], k[1:stumpKeySize])
			seen[xorKey] = struct{}{}
		}
		return nil
	})
	if err != nil {
		slog.Error("DiskStumpStore.Len failed", "error", err)
	}

	return len(seen)
}
