package diskstore

import (
	"encoding/binary"
	"fmt"
	"sync"
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

// BufferedTxidList wraps a TxidList with an in-memory write buffer that
// flushes to disk in batches, avoiding per-append transaction overhead.
type BufferedTxidList struct {
	inner   *TxidList
	buf     []chainhash.Hash
	bufSize int
	mu      sync.Mutex
}

// NewBufferedTxidList creates a BufferedTxidList with the given flush threshold.
func NewBufferedTxidList(db *DB, bufSize int) *BufferedTxidList {
	if bufSize <= 0 {
		bufSize = 1000
	}
	return &BufferedTxidList{
		inner:   NewTxidList(db),
		buf:     make([]chainhash.Hash, 0, bufSize),
		bufSize: bufSize,
	}
}

// Append adds a hash to the buffer. Flushes to disk when the buffer is full.
func (bl *BufferedTxidList) Append(h chainhash.Hash) error {
	bl.mu.Lock()
	bl.buf = append(bl.buf, h)
	if len(bl.buf) >= bl.bufSize {
		err := bl.flushLocked()
		bl.mu.Unlock()
		return err
	}
	bl.mu.Unlock()
	return nil
}

// Flush writes any buffered entries to disk.
func (bl *BufferedTxidList) Flush() error {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return bl.flushLocked()
}

func (bl *BufferedTxidList) flushLocked() error {
	if len(bl.buf) == 0 {
		return nil
	}
	wb := bl.inner.db.db.NewWriteBatch()
	baseIdx := bl.inner.len.Load()
	for i, h := range bl.buf {
		key := makeKey(baseIdx + int64(i))
		val := make([]byte, 32)
		copy(val, h[:])
		if err := wb.Set(key, val); err != nil {
			wb.Cancel()
			return fmt.Errorf("diskstore: buffered_txid_list flush: %w", err)
		}
	}
	if err := wb.Flush(); err != nil {
		return fmt.Errorf("diskstore: buffered_txid_list flush: %w", err)
	}
	bl.inner.len.Add(int64(len(bl.buf)))
	bl.buf = bl.buf[:0]
	return nil
}

// Get retrieves the hash at the given index, checking the buffer first.
func (bl *BufferedTxidList) Get(index int) (chainhash.Hash, bool) {
	bl.mu.Lock()
	diskLen := int(bl.inner.len.Load())
	if index >= diskLen && index < diskLen+len(bl.buf) {
		h := bl.buf[index-diskLen]
		bl.mu.Unlock()
		return h, true
	}
	bl.mu.Unlock()
	return bl.inner.Get(index)
}

// Set overwrites the hash at the given index. Flushes buffer first if needed.
func (bl *BufferedTxidList) Set(index int, h chainhash.Hash) error {
	bl.mu.Lock()
	diskLen := int(bl.inner.len.Load())
	if index >= diskLen && index < diskLen+len(bl.buf) {
		bl.buf[index-diskLen] = h
		bl.mu.Unlock()
		return nil
	}
	bl.mu.Unlock()
	return bl.inner.Set(index, h)
}

// Slice returns hashes in the half-open range [start, end).
// Flushes the buffer first to ensure consistency.
func (bl *BufferedTxidList) Slice(start, end int) []chainhash.Hash {
	bl.mu.Lock()
	_ = bl.flushLocked()
	bl.mu.Unlock()
	return bl.inner.Slice(start, end)
}

// Len returns the total number of entries (disk + buffer).
func (bl *BufferedTxidList) Len() int {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return int(bl.inner.len.Load()) + len(bl.buf)
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
