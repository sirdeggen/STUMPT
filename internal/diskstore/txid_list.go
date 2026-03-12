package diskstore

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// txidListPrefix is the single-byte key namespace for txid list entries.
const txidListPrefix byte = 'x'

// TxidList is a disk-backed ordered list of transaction IDs.
// Keys are formatted as: 'x' (1 byte) + index (8 bytes big-endian).
// Values are 32-byte hashes.
type TxidList struct {
	db  *DB
	len atomic.Int64
}

// NewTxidList creates a TxidList backed by the given DB.
// It scans existing keys to initialise the length counter.
func NewTxidList(db *DB) *TxidList {
	tl := &TxidList{db: db}
	tl.initLen()
	return tl
}

// initLen scans the prefix to find the current number of entries.
func (tl *TxidList) initLen() {
	var count int64
	_ = tl.db.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = []byte{txidListPrefix}
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			count++
		}
		return nil
	})
	tl.len.Store(count)
}

// makeKey builds a 9-byte key: prefix byte + 8-byte big-endian index.
func makeKey(index int64) []byte {
	key := make([]byte, 9)
	key[0] = txidListPrefix
	binary.BigEndian.PutUint64(key[1:], uint64(index))
	return key
}

// Append adds a hash at the next index and atomically increments the length.
func (tl *TxidList) Append(h chainhash.Hash) error {
	idx := tl.len.Load()
	key := makeKey(idx)
	err := tl.db.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, h[:])
	})
	if err != nil {
		return fmt.Errorf("diskstore: txid_list append: %w", err)
	}
	tl.len.Add(1)
	return nil
}

// Get retrieves the hash at the given index. Returns false if out of range.
func (tl *TxidList) Get(index int) (chainhash.Hash, bool) {
	var h chainhash.Hash
	if int64(index) >= tl.len.Load() || index < 0 {
		return h, false
	}
	key := makeKey(int64(index))
	err := tl.db.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			copy(h[:], val)
			return nil
		})
	})
	if err != nil {
		return h, false
	}
	return h, true
}

// Set overwrites the hash at the given index. Returns an error if out of range.
func (tl *TxidList) Set(index int, h chainhash.Hash) error {
	if int64(index) >= tl.len.Load() || index < 0 {
		return fmt.Errorf("diskstore: txid_list set: index %d out of range [0, %d)", index, tl.len.Load())
	}
	key := makeKey(int64(index))
	return tl.db.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, h[:])
	})
}

// Slice returns hashes in the half-open range [start, end).
// Out-of-range indices are clamped silently.
func (tl *TxidList) Slice(start, end int) []chainhash.Hash {
	length := int(tl.len.Load())
	if start < 0 {
		start = 0
	}
	if end > length {
		end = length
	}
	if start >= end {
		return nil
	}

	result := make([]chainhash.Hash, 0, end-start)
	startKey := makeKey(int64(start))
	endKey := makeKey(int64(end))

	_ = tl.db.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte{txidListPrefix}
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(startKey); it.Valid(); it.Next() {
			key := it.Item().Key()
			// Stop when we reach or pass the end key.
			if compareKeys(key, endKey) >= 0 {
				break
			}
			var h chainhash.Hash
			_ = it.Item().Value(func(val []byte) error {
				copy(h[:], val)
				return nil
			})
			result = append(result, h)
		}
		return nil
	})
	return result
}

// Len returns the current number of entries.
func (tl *TxidList) Len() int {
	return int(tl.len.Load())
}

// compareKeys compares two byte slices lexicographically.
func compareKeys(a, b []byte) int {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	for i := 0; i < minLen; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}
