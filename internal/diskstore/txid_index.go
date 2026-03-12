package diskstore

import (
	"log/slog"
	"sync"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// txidPrefix is the single-byte key prefix for txid→token entries.
const txidPrefix = 't'

// DiskTxIDIndex implements stump.TxIDIndexer backed by BadgerDB.
// Keys are 't' (1 byte) + txid (32 bytes); values are the token string as bytes.
type DiskTxIDIndex struct {
	db *DB
}

// NewDiskTxIDIndex creates a new disk-backed txid→token index.
func NewDiskTxIDIndex(db *DB) *DiskTxIDIndex {
	return &DiskTxIDIndex{db: db}
}

// txidKey builds the 33-byte BadgerDB key: 't' + txid.
func txidKey(txid chainhash.Hash) []byte {
	key := make([]byte, 1+chainhash.HashSize)
	key[0] = txidPrefix
	copy(key[1:], txid[:])
	return key
}

// Set records that txid belongs to the given token.
func (d *DiskTxIDIndex) Set(txid chainhash.Hash, token string) {
	key := txidKey(txid)
	val := []byte(token)
	_ = d.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(key, val)
	})
}

// Get returns the token that owns this txid, or ("", false) if not found.
func (d *DiskTxIDIndex) Get(txid chainhash.Hash) (string, bool) {
	key := txidKey(txid)
	var token string
	err := d.db.BadgerDB().View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			token = string(val)
			return nil
		})
	})
	if err != nil {
		return "", false
	}
	return token, true
}

// TxIDTokenPair holds a txid→token pair for batch writes.
type TxIDTokenPair struct {
	TxID  chainhash.Hash
	Token string
}

// SetBatch records multiple txid→token pairs in one write batch.
func (d *DiskTxIDIndex) SetBatch(pairs []TxIDTokenPair) {
	wb := d.db.BadgerDB().NewWriteBatch()
	defer wb.Cancel()
	for _, p := range pairs {
		k := txidKey(p.TxID)
		if err := wb.Set(k, []byte(p.Token)); err != nil {
			slog.Error("diskstore: txid batch set failed", "err", err)
			return
		}
	}
	if err := wb.Flush(); err != nil {
		slog.Error("diskstore: txid batch flush failed", "err", err)
	}
}

// BufferedTxIDIndex wraps DiskTxIDIndex with write buffering.
// Implements stump.TxIDIndexer.
type BufferedTxIDIndex struct {
	inner   *DiskTxIDIndex
	buf     []TxIDTokenPair
	bufSize int
	mu      sync.Mutex
}

// NewBufferedTxIDIndex creates a BufferedTxIDIndex that flushes every bufSize entries.
func NewBufferedTxIDIndex(db *DB, bufSize int) *BufferedTxIDIndex {
	return &BufferedTxIDIndex{
		inner:   NewDiskTxIDIndex(db),
		bufSize: bufSize,
	}
}

// Set buffers a txid→token pair and flushes when the buffer reaches bufSize.
func (b *BufferedTxIDIndex) Set(txid chainhash.Hash, token string) {
	b.mu.Lock()
	b.buf = append(b.buf, TxIDTokenPair{TxID: txid, Token: token})
	if len(b.buf) >= b.bufSize {
		batch := b.buf
		b.buf = make([]TxIDTokenPair, 0, b.bufSize)
		b.mu.Unlock()
		b.inner.SetBatch(batch)
		return
	}
	b.mu.Unlock()
}

// Flush writes any buffered pairs to disk.
func (b *BufferedTxIDIndex) Flush() {
	b.mu.Lock()
	batch := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(batch) > 0 {
		b.inner.SetBatch(batch)
	}
}

// Get checks the buffer first, then falls through to the disk index.
func (b *BufferedTxIDIndex) Get(txid chainhash.Hash) (string, bool) {
	// Check buffer first.
	b.mu.Lock()
	for i := len(b.buf) - 1; i >= 0; i-- {
		if b.buf[i].TxID == txid {
			tok := b.buf[i].Token
			b.mu.Unlock()
			return tok, true
		}
	}
	b.mu.Unlock()
	return b.inner.Get(txid)
}

// Len returns the number of indexed txids on disk.
func (b *BufferedTxIDIndex) Len() int { return b.inner.Len() }

// Len returns the number of indexed txids by prefix-scanning all 't' keys.
// This is expensive and intended for diagnostics only.
func (d *DiskTxIDIndex) Len() int {
	count := 0
	prefix := []byte{txidPrefix}
	_ = d.db.BadgerDB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.Valid(); it.Next() {
			count++
		}
		return nil
	})
	return count
}
