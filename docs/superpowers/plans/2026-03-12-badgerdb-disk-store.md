# BadgerDB Disk-Backed STUMP Store

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace in-memory maps that grow linearly with txid count (`stumpStore`, `txidIndex`, `allTxids`, `tokenProofs`, `minerSubtrees` internals) with BadgerDB-backed disk storage, reducing peak RAM from O(txids) to O(hashesPerSubtree).

**Architecture:** Introduce a `diskstore` package wrapping BadgerDB with typed accessors for each data class. The stump `Store` and `TxIDIndex` interfaces stay the same but are backed by BadgerDB prefix-scanned keys. `allTxids` becomes a rolling buffer that flushes to disk after each subtree seal. MinerSubtree leaf/store arrays are serialized to BadgerDB after sealing and only reloaded for coinbase reseal of subtree-0. A single BadgerDB instance is shared across all stores using key prefixes to namespace the data.

**Tech Stack:** Go 1.24, BadgerDB v4, existing `chainhash.Hash` types, `encoding/binary` for serialization.

---

## File Structure

| File | Responsibility |
|------|---------------|
| `internal/diskstore/db.go` | BadgerDB lifecycle (open, close, temp dir), shared instance |
| `internal/diskstore/stump_store.go` | Disk-backed `stump.Store` replacement — XOR key prefix scan |
| `internal/diskstore/txid_index.go` | Disk-backed `stump.TxIDIndex` replacement — point lookups |
| `internal/diskstore/txid_list.go` | Disk-backed ordered txid list (replaces `allTxids` slice) |
| `internal/diskstore/miner_subtree.go` | Serialize/deserialize MinerSubtree Leaves+Store to disk |
| `internal/diskstore/encoding.go` | Binary serialization for Entry, chainhash slices |
| `internal/diskstore/db_test.go` | DB lifecycle tests |
| `internal/diskstore/stump_store_test.go` | Store correctness + benchmark vs in-memory |
| `internal/diskstore/txid_index_test.go` | TxIDIndex correctness |
| `internal/diskstore/txid_list_test.go` | TxidList correctness |
| `internal/diskstore/encoding_test.go` | Round-trip serialization tests |
| `internal/stump/stump.go` | Extract `StumpStore` and `TxIDIndexer` interfaces |
| `internal/merkleservice/registry.go` | Swap concrete types for interfaces, accept `diskstore.DB` |
| `internal/config/config.go` | Add `DataDir string` field |
| `cmd/harness/main.go` | Open/close BadgerDB, pass to registry |

---

## Chunk 1: Foundation — BadgerDB, Encoding, Interfaces

### Task 1: Add BadgerDB dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add badger v4**

```bash
cd /Users/personal/git/STUMPT && go get github.com/dgraph-io/badger/v4
```

- [ ] **Step 2: Verify**

```bash
cd /Users/personal/git/STUMPT && go mod tidy && grep badger go.mod
```

Expected: `github.com/dgraph-io/badger/v4 v4.x.x`

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add badger/v4 for disk-backed STUMP storage"
```

---

### Task 2: Binary encoding helpers

**Files:**
- Create: `internal/diskstore/encoding.go`
- Create: `internal/diskstore/encoding_test.go`

- [ ] **Step 1: Write encoding round-trip test**

```go
// internal/diskstore/encoding_test.go
package diskstore_test

import (
	"crypto/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func randomHash() chainhash.Hash {
	var h chainhash.Hash
	if _, err := rand.Read(h[:]); err != nil {
		panic(err)
	}
	return h
}

func TestEntryRoundTrip(t *testing.T) {
	sib1, sib2 := randomHash(), randomHash()
	original := &stump.Entry{
		TxID:        randomHash(),
		SubtreeIdx:  42,
		LocalIdx:    7,
		GlobalIdx:   42*64 + 7,
		SiblingPath: []*chainhash.Hash{&sib1, &sib2},
	}

	data := diskstore.MarshalEntry(original)
	got, err := diskstore.UnmarshalEntry(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.TxID != original.TxID {
		t.Fatal("TxID mismatch")
	}
	if got.SubtreeIdx != original.SubtreeIdx || got.LocalIdx != original.LocalIdx || got.GlobalIdx != original.GlobalIdx {
		t.Fatal("index mismatch")
	}
	if len(got.SiblingPath) != len(original.SiblingPath) {
		t.Fatalf("sibling path length: got %d want %d", len(got.SiblingPath), len(original.SiblingPath))
	}
	for i := range got.SiblingPath {
		if *got.SiblingPath[i] != *original.SiblingPath[i] {
			t.Fatalf("sibling %d mismatch", i)
		}
	}
}

func TestEntriesRoundTrip(t *testing.T) {
	entries := make([]*stump.Entry, 5)
	for i := range entries {
		sib := randomHash()
		entries[i] = &stump.Entry{
			TxID:        randomHash(),
			SubtreeIdx:  i,
			LocalIdx:    i * 2,
			GlobalIdx:   i*64 + i*2,
			SiblingPath: []*chainhash.Hash{&sib},
		}
	}
	data := diskstore.MarshalEntries(entries)
	got, err := diskstore.UnmarshalEntries(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(entries) {
		t.Fatalf("count: got %d want %d", len(got), len(entries))
	}
}

func TestHashSliceRoundTrip(t *testing.T) {
	hashes := make([]chainhash.Hash, 10)
	for i := range hashes {
		hashes[i] = randomHash()
	}
	data := diskstore.MarshalHashes(hashes)
	got, err := diskstore.UnmarshalHashes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(hashes) {
		t.Fatalf("count: got %d want %d", len(got), len(hashes))
	}
	for i := range got {
		if got[i] != hashes[i] {
			t.Fatalf("hash %d mismatch", i)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestEntry
```

Expected: compilation error (package doesn't exist)

- [ ] **Step 3: Implement encoding**

```go
// internal/diskstore/encoding.go
package diskstore

import (
	"encoding/binary"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

// MarshalEntry serializes a stump.Entry to bytes.
// Format: [32B TxID][4B SubtreeIdx][4B LocalIdx][4B GlobalIdx][4B numSiblings][32B * siblings...]
func MarshalEntry(e *stump.Entry) []byte {
	n := 32 + 4 + 4 + 4 + 4 + 32*len(e.SiblingPath)
	buf := make([]byte, n)
	off := 0

	copy(buf[off:], e.TxID[:])
	off += 32

	binary.LittleEndian.PutUint32(buf[off:], uint32(e.SubtreeIdx))
	off += 4
	binary.LittleEndian.PutUint32(buf[off:], uint32(e.LocalIdx))
	off += 4
	binary.LittleEndian.PutUint32(buf[off:], uint32(e.GlobalIdx))
	off += 4

	binary.LittleEndian.PutUint32(buf[off:], uint32(len(e.SiblingPath)))
	off += 4

	for _, h := range e.SiblingPath {
		copy(buf[off:], h[:])
		off += 32
	}
	return buf
}

// UnmarshalEntry deserializes bytes into a stump.Entry.
func UnmarshalEntry(data []byte) (*stump.Entry, error) {
	if len(data) < 48 { // 32 + 4 + 4 + 4 + 4
		return nil, fmt.Errorf("entry too short: %d bytes", len(data))
	}
	e := &stump.Entry{}
	off := 0

	copy(e.TxID[:], data[off:off+32])
	off += 32

	e.SubtreeIdx = int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	e.LocalIdx = int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	e.GlobalIdx = int(binary.LittleEndian.Uint32(data[off:]))
	off += 4

	numSib := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4

	need := off + numSib*32
	if len(data) < need {
		return nil, fmt.Errorf("entry truncated: need %d, got %d", need, len(data))
	}

	e.SiblingPath = make([]*chainhash.Hash, numSib)
	for i := 0; i < numSib; i++ {
		h := new(chainhash.Hash)
		copy(h[:], data[off:off+32])
		off += 32
		e.SiblingPath[i] = h
	}
	return e, nil
}

// MarshalEntries serializes a slice of entries.
// Format: [4B count][entry1 bytes...][entry2 bytes...]...
// Each entry is length-prefixed with 4B.
func MarshalEntries(entries []*stump.Entry) []byte {
	// Pre-compute total size.
	parts := make([][]byte, len(entries))
	total := 4 // count header
	for i, e := range entries {
		parts[i] = MarshalEntry(e)
		total += 4 + len(parts[i]) // length prefix + data
	}

	buf := make([]byte, total)
	off := 0
	binary.LittleEndian.PutUint32(buf[off:], uint32(len(entries)))
	off += 4

	for _, p := range parts {
		binary.LittleEndian.PutUint32(buf[off:], uint32(len(p)))
		off += 4
		copy(buf[off:], p)
		off += len(p)
	}
	return buf
}

// UnmarshalEntries deserializes a slice of entries.
func UnmarshalEntries(data []byte) ([]*stump.Entry, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("entries too short")
	}
	count := int(binary.LittleEndian.Uint32(data[:4]))
	off := 4
	entries := make([]*stump.Entry, count)

	for i := 0; i < count; i++ {
		if off+4 > len(data) {
			return nil, fmt.Errorf("truncated at entry %d", i)
		}
		size := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if off+size > len(data) {
			return nil, fmt.Errorf("entry %d: need %d bytes, %d available", i, size, len(data)-off)
		}
		e, err := UnmarshalEntry(data[off : off+size])
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		entries[i] = e
		off += size
	}
	return entries, nil
}

// MarshalHashes serializes a []chainhash.Hash.
// Format: [4B count][32B hash]...
func MarshalHashes(hashes []chainhash.Hash) []byte {
	buf := make([]byte, 4+32*len(hashes))
	binary.LittleEndian.PutUint32(buf, uint32(len(hashes)))
	for i, h := range hashes {
		copy(buf[4+i*32:], h[:])
	}
	return buf
}

// UnmarshalHashes deserializes a []chainhash.Hash.
func UnmarshalHashes(data []byte) ([]chainhash.Hash, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("hashes too short")
	}
	count := int(binary.LittleEndian.Uint32(data[:4]))
	if len(data) < 4+count*32 {
		return nil, fmt.Errorf("hashes truncated")
	}
	hashes := make([]chainhash.Hash, count)
	for i := 0; i < count; i++ {
		copy(hashes[i][:], data[4+i*32:])
	}
	return hashes, nil
}
```

- [ ] **Step 4: Run tests**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run "TestEntry|TestHash"
```

Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/diskstore/
git commit -m "feat: add binary encoding for stump entries and hash slices"
```

---

### Task 3: BadgerDB lifecycle manager

**Files:**
- Create: `internal/diskstore/db.go`
- Create: `internal/diskstore/db_test.go`

- [ ] **Step 1: Write DB lifecycle test**

```go
// internal/diskstore/db_test.go
package diskstore_test

import (
	"testing"

	"github.com/bsv-blockchain/stumpt/internal/diskstore"
)

func TestDBOpenClose(t *testing.T) {
	db, err := diskstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.BadgerDB() == nil {
		t.Fatal("expected non-nil badger.DB")
	}
}

func TestDBDoubleClose(t *testing.T) {
	db, err := diskstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Second close should not panic.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestDB
```

Expected: compilation error

- [ ] **Step 3: Implement DB**

```go
// internal/diskstore/db.go
package diskstore

import (
	"os"
	"sync"

	badger "github.com/dgraph-io/badger/v4"
)

// DB wraps a BadgerDB instance with lifecycle management.
// Pass dir="" for a temp directory (cleaned up on Close).
type DB struct {
	db      *badger.DB
	dir     string
	tempDir bool
	once    sync.Once
}

// Open creates or opens a BadgerDB. If dir is empty, a temp directory is used.
func Open(dir string) (*DB, error) {
	temp := dir == ""
	if temp {
		var err error
		dir, err = os.MkdirTemp("", "stumpt-badger-*")
		if err != nil {
			return nil, err
		}
	}

	opts := badger.DefaultOptions(dir).
		WithLogger(nil). // suppress badger logs
		WithNumVersionsToKeep(1).
		WithCompactL0OnClose(false).
		WithNumCompactors(2)

	bdb, err := badger.Open(opts)
	if err != nil {
		if temp {
			os.RemoveAll(dir)
		}
		return nil, err
	}

	return &DB{db: bdb, dir: dir, tempDir: temp}, nil
}

// BadgerDB returns the underlying badger.DB for direct access.
func (d *DB) BadgerDB() *badger.DB { return d.db }

// Close closes BadgerDB and removes the temp dir if applicable.
func (d *DB) Close() error {
	var err error
	d.once.Do(func() {
		err = d.db.Close()
		if d.tempDir {
			os.RemoveAll(d.dir)
		}
	})
	return err
}
```

- [ ] **Step 4: Run tests**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestDB
```

Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/diskstore/
git commit -m "feat: add BadgerDB lifecycle manager with temp dir support"
```

---

### Task 4: Extract Store and TxIDIndexer interfaces from stump package

The current `stump.Store` and `stump.TxIDIndex` are concrete types. We need interfaces so the registry can accept either in-memory or disk-backed implementations.

**Files:**
- Modify: `internal/stump/stump.go`

- [ ] **Step 1: Add interfaces above the concrete types**

Add these interfaces at the top of `stump.go`, just after the `XORKey` and `TokenHash` functions (after line 89):

```go
// StumpStore is the interface for storing and retrieving STUMP entries by XOR key.
type StumpStore interface {
	Append(key Key, e *Entry)
	AppendBatch(key Key, entries []*Entry)
	Get(key Key) []*Entry
	Len() int
}

// TxIDIndexer is the interface for the txid→token reverse index.
type TxIDIndexer interface {
	Set(txid chainhash.Hash, token string)
	Get(txid chainhash.Hash) (string, bool)
	Len() int
}
```

- [ ] **Step 2: Update Discover to accept interfaces**

Change the `Discover` function signature from:

```go
func Discover(store *Store, registry *TokenRegistry, subtreeRoots []chainhash.Hash) map[string][]*Entry {
```

to:

```go
func Discover(store StumpStore, registry *TokenRegistry, subtreeRoots []chainhash.Hash) map[string][]*Entry {
```

- [ ] **Step 3: Verify existing tests still pass**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/stump/ ./internal/merkleservice/ -v
```

Expected: all PASS (concrete types satisfy the interfaces implicitly)

- [ ] **Step 4: Update registry.go to use interfaces**

In `internal/merkleservice/registry.go`, change the field types:

```go
// Old:
stumpStore *stump.Store
txidIndex  *stump.TxIDIndex

// New:
stumpStore stump.StumpStore
txidIndex  stump.TxIDIndexer
```

- [ ] **Step 5: Verify all tests pass**

```bash
cd /Users/personal/git/STUMPT && go test ./... -v
```

Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/stump/stump.go internal/merkleservice/registry.go
git commit -m "refactor: extract StumpStore and TxIDIndexer interfaces for pluggable backends"
```

---

## Chunk 2: Disk-Backed Stores

### Task 5: Disk-backed StumpStore

**Files:**
- Create: `internal/diskstore/stump_store.go`
- Create: `internal/diskstore/stump_store_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/diskstore/stump_store_test.go
package diskstore_test

import (
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func openTestDB(t *testing.T) *diskstore.DB {
	t.Helper()
	db, err := diskstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDiskStoreAppendAndGet(t *testing.T) {
	db := openTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key := randomHash()
	sib := randomHash()
	e := &stump.Entry{
		TxID:        randomHash(),
		SubtreeIdx:  5,
		LocalIdx:    42,
		GlobalIdx:   5*64 + 42,
		SiblingPath: []*chainhash.Hash{&sib},
	}

	store.Append(key, e)

	got := store.Get(key)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].TxID != e.TxID {
		t.Fatal("TxID mismatch")
	}
	if got[0].SubtreeIdx != 5 || got[0].LocalIdx != 42 {
		t.Fatal("index mismatch")
	}
	if len(got[0].SiblingPath) != 1 || *got[0].SiblingPath[0] != sib {
		t.Fatal("sibling path mismatch")
	}
}

func TestDiskStoreAppendBatch(t *testing.T) {
	db := openTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key := randomHash()
	entries := make([]*stump.Entry, 10)
	for i := range entries {
		entries[i] = &stump.Entry{TxID: randomHash(), SubtreeIdx: 0, LocalIdx: i}
	}

	store.AppendBatch(key, entries)

	got := store.Get(key)
	if len(got) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(got))
	}
}

func TestDiskStoreMissReturnsNil(t *testing.T) {
	db := openTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	if got := store.Get(randomHash()); got != nil {
		t.Fatal("expected nil for missing key")
	}
}

func TestDiskStoreMultipleAppendsAccumulate(t *testing.T) {
	db := openTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key := randomHash()
	store.Append(key, &stump.Entry{TxID: randomHash(), SubtreeIdx: 0})
	store.Append(key, &stump.Entry{TxID: randomHash(), SubtreeIdx: 1})
	store.Append(key, &stump.Entry{TxID: randomHash(), SubtreeIdx: 2})

	got := store.Get(key)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
}

func TestDiskStoreLen(t *testing.T) {
	db := openTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	k1, k2 := randomHash(), randomHash()
	store.Append(k1, &stump.Entry{TxID: randomHash()})
	store.Append(k1, &stump.Entry{TxID: randomHash()})
	store.Append(k2, &stump.Entry{TxID: randomHash()})

	// Len counts unique keys.
	if store.Len() != 2 {
		t.Fatalf("expected 2 unique keys, got %d", store.Len())
	}
}

// TestDiskStoreInterfaceCompliance verifies the disk store satisfies StumpStore.
func TestDiskStoreInterfaceCompliance(t *testing.T) {
	db := openTestDB(t)
	var _ stump.StumpStore = diskstore.NewDiskStumpStore(db)
}

// BenchmarkDiskStoreAppend measures disk-backed append throughput.
func BenchmarkDiskStoreAppend(b *testing.B) {
	db, err := diskstore.Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	store := diskstore.NewDiskStumpStore(db)
	key := randomHash()
	e := &stump.Entry{TxID: randomHash()}
	b.ResetTimer()
	for range b.N {
		store.Append(key, e)
	}
}

// BenchmarkDiskStoreGet measures disk-backed lookup throughput.
func BenchmarkDiskStoreGet(b *testing.B) {
	db, err := diskstore.Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	store := diskstore.NewDiskStumpStore(db)
	key := randomHash()
	store.Append(key, &stump.Entry{TxID: randomHash()})
	b.ResetTimer()
	for range b.N {
		_ = store.Get(key)
	}
}
```

- [ ] **Step 2: Run test to verify compilation fails**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestDiskStore
```

Expected: compilation error (NewDiskStumpStore doesn't exist)

- [ ] **Step 3: Implement DiskStumpStore**

Key design: each entry gets its own BadgerDB key = `s` + [32B XOR key] + [8B sequence]. Get does a prefix scan on `s` + [32B XOR key]. This avoids read-modify-write on Append and gives good write throughput.

```go
// internal/diskstore/stump_store.go
package diskstore

import (
	"encoding/binary"
	"log/slog"
	"sync/atomic"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

const stumpPrefix = 's'

// DiskStumpStore implements stump.StumpStore backed by BadgerDB.
// Keys are: 's' + [32B XOR key] + [8B sequence number].
// Values are: serialized stump.Entry.
type DiskStumpStore struct {
	db  *DB
	seq atomic.Uint64
}

// NewDiskStumpStore creates a new disk-backed STUMP store.
func NewDiskStumpStore(db *DB) *DiskStumpStore {
	return &DiskStumpStore{db: db}
}

func stumpKey(xorKey chainhash.Hash, seq uint64) []byte {
	k := make([]byte, 1+32+8)
	k[0] = stumpPrefix
	copy(k[1:33], xorKey[:])
	binary.BigEndian.PutUint64(k[33:], seq)
	return k
}

func stumpKeyPrefix(xorKey chainhash.Hash) []byte {
	k := make([]byte, 1+32)
	k[0] = stumpPrefix
	copy(k[1:33], xorKey[:])
	return k
}

// Append adds a single entry under the given XOR key.
func (s *DiskStumpStore) Append(key stump.Key, e *stump.Entry) {
	seq := s.seq.Add(1)
	k := stumpKey(key, seq)
	v := MarshalEntry(e)

	err := s.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(k, v)
	})
	if err != nil {
		slog.Error("diskstore: stump append failed", "err", err)
	}
}

// AppendBatch adds multiple entries under the given XOR key in one transaction.
func (s *DiskStumpStore) AppendBatch(key stump.Key, entries []*stump.Entry) {
	if len(entries) == 0 {
		return
	}

	wb := s.db.BadgerDB().NewWriteBatch()
	defer wb.Cancel()

	for _, e := range entries {
		seq := s.seq.Add(1)
		k := stumpKey(key, seq)
		v := MarshalEntry(e)
		if err := wb.Set(k, v); err != nil {
			slog.Error("diskstore: stump batch set failed", "err", err)
			return
		}
	}

	if err := wb.Flush(); err != nil {
		slog.Error("diskstore: stump batch flush failed", "err", err)
	}
}

// Get returns all entries stored under the given XOR key via prefix scan.
func (s *DiskStumpStore) Get(key stump.Key) []*stump.Entry {
	prefix := stumpKeyPrefix(key)
	var entries []*stump.Entry

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
					return err
				}
				entries = append(entries, e)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("diskstore: stump get failed", "err", err)
		return nil
	}

	if len(entries) == 0 {
		return nil
	}
	return entries
}

// Len returns the number of unique XOR keys in the store.
// Note: this is expensive (full scan) — use only for diagnostics.
func (s *DiskStumpStore) Len() int {
	count := 0
	var lastKey [32]byte
	hasLast := false

	_ = s.db.BadgerDB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = []byte{stumpPrefix}
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek([]byte{stumpPrefix}); it.ValidForPrefix([]byte{stumpPrefix}); it.Next() {
			k := it.Item().Key()
			if len(k) < 33 {
				continue
			}
			var thisKey [32]byte
			copy(thisKey[:], k[1:33])
			if !hasLast || thisKey != lastKey {
				count++
				lastKey = thisKey
				hasLast = true
			}
		}
		return nil
	})

	return count
}
```

- [ ] **Step 4: Run tests**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestDiskStore -count=1
```

Expected: all PASS

- [ ] **Step 5: Run benchmarks**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -bench BenchmarkDiskStore -benchmem -count=3
```

Record the numbers for comparison.

- [ ] **Step 6: Commit**

```bash
git add internal/diskstore/stump_store.go internal/diskstore/stump_store_test.go
git commit -m "feat: disk-backed DiskStumpStore with prefix-scan Get and WriteBatch Append"
```

---

### Task 6: Disk-backed TxIDIndex

**Files:**
- Create: `internal/diskstore/txid_index.go`
- Create: `internal/diskstore/txid_index_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/diskstore/txid_index_test.go
package diskstore_test

import (
	"testing"

	"github.com/bsv-blockchain/stumpt/internal/diskstore"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func TestDiskTxIDIndexSetGet(t *testing.T) {
	db := openTestDB(t)
	idx := diskstore.NewDiskTxIDIndex(db)

	txid := randomHash()
	idx.Set(txid, "token-7")

	tok, ok := idx.Get(txid)
	if !ok || tok != "token-7" {
		t.Fatalf("expected token-7, got %q (ok=%v)", tok, ok)
	}
}

func TestDiskTxIDIndexMiss(t *testing.T) {
	db := openTestDB(t)
	idx := diskstore.NewDiskTxIDIndex(db)

	_, ok := idx.Get(randomHash())
	if ok {
		t.Fatal("expected miss for unknown txid")
	}
}

func TestDiskTxIDIndexInterfaceCompliance(t *testing.T) {
	db := openTestDB(t)
	var _ stump.TxIDIndexer = diskstore.NewDiskTxIDIndex(db)
}

func BenchmarkDiskTxIDIndexSet(b *testing.B) {
	db, _ := diskstore.Open("")
	defer db.Close()
	idx := diskstore.NewDiskTxIDIndex(db)
	b.ResetTimer()
	for range b.N {
		idx.Set(randomHash(), "token-0")
	}
}

func BenchmarkDiskTxIDIndexGet(b *testing.B) {
	db, _ := diskstore.Open("")
	defer db.Close()
	idx := diskstore.NewDiskTxIDIndex(db)
	txid := randomHash()
	idx.Set(txid, "token-0")
	b.ResetTimer()
	for range b.N {
		idx.Get(txid)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestDiskTxID
```

- [ ] **Step 3: Implement DiskTxIDIndex**

```go
// internal/diskstore/txid_index.go
package diskstore

import (
	"log/slog"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/bsv-blockchain/go-sdk/chainhash"
)

const txidPrefix = 't'

// DiskTxIDIndex implements stump.TxIDIndexer backed by BadgerDB.
// Key: 't' + [32B txid], Value: token string bytes.
type DiskTxIDIndex struct {
	db *DB
}

// NewDiskTxIDIndex creates a new disk-backed txid→token index.
func NewDiskTxIDIndex(db *DB) *DiskTxIDIndex {
	return &DiskTxIDIndex{db: db}
}

func txidKey(txid chainhash.Hash) []byte {
	k := make([]byte, 1+32)
	k[0] = txidPrefix
	copy(k[1:], txid[:])
	return k
}

// Set records that txid belongs to the given token.
func (d *DiskTxIDIndex) Set(txid chainhash.Hash, token string) {
	k := txidKey(txid)
	err := d.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(k, []byte(token))
	})
	if err != nil {
		slog.Error("diskstore: txid index set failed", "err", err)
	}
}

// Get returns the token for this txid, or ("", false) if not found.
func (d *DiskTxIDIndex) Get(txid chainhash.Hash) (string, bool) {
	k := txidKey(txid)
	var token string

	err := d.db.BadgerDB().View(func(txn *badger.Txn) error {
		item, err := txn.Get(k)
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

// Len returns the number of indexed txids.
func (d *DiskTxIDIndex) Len() int {
	count := 0
	_ = d.db.BadgerDB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = []byte{txidPrefix}
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek([]byte{txidPrefix}); it.ValidForPrefix([]byte{txidPrefix}); it.Next() {
			count++
		}
		return nil
	})
	return count
}
```

- [ ] **Step 4: Run tests**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestDiskTxID
```

Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/diskstore/txid_index.go internal/diskstore/txid_index_test.go
git commit -m "feat: disk-backed DiskTxIDIndex for txid→token lookups"
```

---

### Task 7: Disk-backed TxidList (replaces allTxids slice)

**Files:**
- Create: `internal/diskstore/txid_list.go`
- Create: `internal/diskstore/txid_list_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/diskstore/txid_list_test.go
package diskstore_test

import (
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
)

func TestTxidListAppendAndGet(t *testing.T) {
	db := openTestDB(t)
	list := diskstore.NewTxidList(db)

	h1, h2, h3 := randomHash(), randomHash(), randomHash()
	list.Append(h1)
	list.Append(h2)
	list.Append(h3)

	if list.Len() != 3 {
		t.Fatalf("expected 3, got %d", list.Len())
	}

	got, ok := list.Get(0)
	if !ok || got != h1 {
		t.Fatal("index 0 mismatch")
	}
	got, ok = list.Get(2)
	if !ok || got != h3 {
		t.Fatal("index 2 mismatch")
	}
	_, ok = list.Get(99)
	if ok {
		t.Fatal("expected miss for out-of-range index")
	}
}

func TestTxidListSet(t *testing.T) {
	db := openTestDB(t)
	list := diskstore.NewTxidList(db)

	h1, h2 := randomHash(), randomHash()
	list.Append(h1)

	list.Set(0, h2)
	got, _ := list.Get(0)
	if got != h2 {
		t.Fatal("Set did not update")
	}
}

func TestTxidListSlice(t *testing.T) {
	db := openTestDB(t)
	list := diskstore.NewTxidList(db)

	hashes := make([]chainhash.Hash, 10)
	for i := range hashes {
		hashes[i] = randomHash()
		list.Append(hashes[i])
	}

	slice := list.Slice(3, 7)
	if len(slice) != 4 {
		t.Fatalf("expected 4, got %d", len(slice))
	}
	for i, h := range slice {
		if h != hashes[3+i] {
			t.Fatalf("slice[%d] mismatch", i)
		}
	}
}

func BenchmarkTxidListAppend(b *testing.B) {
	db, _ := diskstore.Open("")
	defer db.Close()
	list := diskstore.NewTxidList(db)
	h := randomHash()
	b.ResetTimer()
	for range b.N {
		list.Append(h)
	}
}
```

- [ ] **Step 2: Implement TxidList**

```go
// internal/diskstore/txid_list.go
package diskstore

import (
	"encoding/binary"
	"log/slog"
	"sync/atomic"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/bsv-blockchain/go-sdk/chainhash"
)

const txidListPrefix = 'x'

// TxidList is a disk-backed ordered list of txids, replacing the allTxids slice.
// Key: 'x' + [8B big-endian index], Value: [32B hash].
type TxidList struct {
	db  *DB
	len atomic.Int64
}

// NewTxidList creates a new disk-backed txid list.
func NewTxidList(db *DB) *TxidList {
	return &TxidList{db: db}
}

func txidListKey(index int) []byte {
	k := make([]byte, 9)
	k[0] = txidListPrefix
	binary.BigEndian.PutUint64(k[1:], uint64(index))
	return k
}

// Append adds a txid to the end of the list.
func (l *TxidList) Append(h chainhash.Hash) {
	idx := int(l.len.Add(1) - 1)
	k := txidListKey(idx)

	err := l.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(k, h[:])
	})
	if err != nil {
		slog.Error("diskstore: txid list append failed", "err", err)
	}
}

// Get retrieves the txid at the given index.
func (l *TxidList) Get(index int) (chainhash.Hash, bool) {
	if index < 0 || index >= int(l.len.Load()) {
		return chainhash.Hash{}, false
	}

	k := txidListKey(index)
	var h chainhash.Hash

	err := l.db.BadgerDB().View(func(txn *badger.Txn) error {
		item, err := txn.Get(k)
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			copy(h[:], val)
			return nil
		})
	})
	if err != nil {
		return chainhash.Hash{}, false
	}
	return h, true
}

// Set overwrites the txid at the given index (used for coinbase replacement).
func (l *TxidList) Set(index int, h chainhash.Hash) {
	k := txidListKey(index)
	err := l.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(k, h[:])
	})
	if err != nil {
		slog.Error("diskstore: txid list set failed", "err", err)
	}
}

// Slice returns txids in [start, end) as a slice (loaded from disk).
func (l *TxidList) Slice(start, end int) []chainhash.Hash {
	if start < 0 {
		start = 0
	}
	ln := int(l.len.Load())
	if end > ln {
		end = ln
	}
	if start >= end {
		return nil
	}

	result := make([]chainhash.Hash, 0, end-start)

	startKey := txidListKey(start)
	endKey := txidListKey(end)

	_ = l.db.BadgerDB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte{txidListPrefix}
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(startKey); it.Valid(); it.Next() {
			item := it.Item()
			k := item.Key()
			if len(k) != 9 {
				continue
			}
			// Stop at endKey.
			if bytesGE(k, endKey) {
				break
			}
			_ = item.Value(func(val []byte) error {
				var h chainhash.Hash
				copy(h[:], val)
				result = append(result, h)
				return nil
			})
		}
		return nil
	})

	return result
}

// Len returns the number of appended txids.
func (l *TxidList) Len() int {
	return int(l.len.Load())
}

func bytesGE(a, b []byte) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return false
		}
		if a[i] > b[i] {
			return true
		}
	}
	return len(a) >= len(b)
}
```

- [ ] **Step 3: Run tests**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestTxidList
```

Expected: all PASS

- [ ] **Step 4: Commit**

```bash
git add internal/diskstore/txid_list.go internal/diskstore/txid_list_test.go
git commit -m "feat: disk-backed TxidList replaces allTxids slice"
```

---

### Task 8: Disk-backed MinerSubtree storage

After sealing, MinerSubtree.Leaves and .Store consume most RAM. We serialize them to disk and only reload for coinbase reseal.

**Files:**
- Create: `internal/diskstore/miner_subtree.go`
- Create: `internal/diskstore/miner_subtree_test.go`

- [ ] **Step 1: Write failing test**

```go
// internal/diskstore/miner_subtree_test.go
package diskstore_test

import (
	"testing"

	"github.com/bsv-blockchain/stumpt/internal/diskstore"
)

func TestMinerSubtreeStoreRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ms := diskstore.NewMinerSubtreeStore(db)

	leaves := make([]byte, 0, 64*32)
	hashes := make([]byte, 0, 127*32)
	for i := 0; i < 64; i++ {
		h := randomHash()
		leaves = append(leaves, h[:]...)
	}
	for i := 0; i < 127; i++ {
		h := randomHash()
		hashes = append(hashes, h[:]...)
	}

	ms.Save(0, 3, leaves, hashes) // miner=0, subtreeIdx=3

	gotLeaves, gotHashes, ok := ms.Load(0, 3)
	if !ok {
		t.Fatal("expected to find saved subtree")
	}
	if len(gotLeaves) != len(leaves) {
		t.Fatalf("leaves length mismatch: %d vs %d", len(gotLeaves), len(leaves))
	}
	if len(gotHashes) != len(hashes) {
		t.Fatalf("hashes length mismatch: %d vs %d", len(gotHashes), len(hashes))
	}

	_, _, ok = ms.Load(1, 3)
	if ok {
		t.Fatal("expected miss for unsaved miner")
	}
}
```

- [ ] **Step 2: Implement MinerSubtreeStore**

```go
// internal/diskstore/miner_subtree.go
package diskstore

import (
	"encoding/binary"
	"log/slog"

	badger "github.com/dgraph-io/badger/v4"
)

const minerSubtreePrefix = 'm'

// MinerSubtreeStore persists MinerSubtree Leaves and Store arrays to disk.
// Key: 'm' + [4B minerIdx] + [4B subtreeIdx] + [1B type: 'L' or 'S']
type MinerSubtreeStore struct {
	db *DB
}

func NewMinerSubtreeStore(db *DB) *MinerSubtreeStore {
	return &MinerSubtreeStore{db: db}
}

func minerSubtreeKey(minerIdx, subtreeIdx int, typ byte) []byte {
	k := make([]byte, 1+4+4+1)
	k[0] = minerSubtreePrefix
	binary.BigEndian.PutUint32(k[1:], uint32(minerIdx))
	binary.BigEndian.PutUint32(k[5:], uint32(subtreeIdx))
	k[9] = typ
	return k
}

// Save persists the raw leaves and store bytes for a miner's subtree.
func (m *MinerSubtreeStore) Save(minerIdx, subtreeIdx int, leaves, store []byte) {
	lk := minerSubtreeKey(minerIdx, subtreeIdx, 'L')
	sk := minerSubtreeKey(minerIdx, subtreeIdx, 'S')

	err := m.db.BadgerDB().Update(func(txn *badger.Txn) error {
		if err := txn.Set(lk, leaves); err != nil {
			return err
		}
		return txn.Set(sk, store)
	})
	if err != nil {
		slog.Error("diskstore: miner subtree save failed", "err", err)
	}
}

// Load retrieves the leaves and store bytes. Returns false if not found.
func (m *MinerSubtreeStore) Load(minerIdx, subtreeIdx int) (leaves, store []byte, ok bool) {
	lk := minerSubtreeKey(minerIdx, subtreeIdx, 'L')
	sk := minerSubtreeKey(minerIdx, subtreeIdx, 'S')

	err := m.db.BadgerDB().View(func(txn *badger.Txn) error {
		item, err := txn.Get(lk)
		if err != nil {
			return err
		}
		leaves, err = item.ValueCopy(nil)
		if err != nil {
			return err
		}

		item, err = txn.Get(sk)
		if err != nil {
			return err
		}
		store, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		return nil, nil, false
	}
	return leaves, store, true
}
```

- [ ] **Step 3: Run tests**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -v -run TestMinerSubtree
```

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/diskstore/miner_subtree.go internal/diskstore/miner_subtree_test.go
git commit -m "feat: disk-backed MinerSubtreeStore for evicting leaf/store arrays"
```

---

## Chunk 3: Integration — Wire Into Registry

### Task 9: Add DataDir to config and open BadgerDB in harness

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/harness/main.go`

- [ ] **Step 1: Add DataDir field to Config**

In `internal/config/config.go`, add to the Config struct:

```go
// DataDir is the directory for BadgerDB storage.
// Empty string uses a temp directory (cleaned up on exit).
DataDir string
```

- [ ] **Step 2: Add -data-dir flag in main.go**

In `cmd/harness/main.go`, add after the existing flag definitions:

```go
flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir,
    "BadgerDB data directory (empty = temp dir, cleaned on exit)")
```

- [ ] **Step 3: Open and close DB in both run modes**

In `runDirect`, after the config validation, add:

```go
db, err := diskstore.Open(cfg.DataDir)
if err != nil {
    slog.Error("badgerdb: open failed", "err", err)
    os.Exit(1)
}
defer db.Close()
```

Pass `db` to `merkleservice.NewServer`. Same pattern in `runHTTP`.

- [ ] **Step 4: Update NewServer signature**

In `internal/merkleservice/server.go`, update `NewServer` to accept `*diskstore.DB` and pass it to `newRegistry`.

- [ ] **Step 5: Verify compilation**

```bash
cd /Users/personal/git/STUMPT && go build ./cmd/harness/
```

Expected: compiles clean

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go cmd/harness/main.go internal/merkleservice/server.go
git commit -m "feat: wire BadgerDB lifecycle into harness and server"
```

---

### Task 10: Swap registry internals for disk-backed stores

This is the core integration task. The registry stops using `[]chainhash.Hash` for allTxids, uses interface types for stumpStore and txidIndex, and evicts MinerSubtree internals after sealing.

**Files:**
- Modify: `internal/merkleservice/registry.go`

- [ ] **Step 1: Update newRegistry to create disk-backed stores**

Replace the concrete store creation in `newRegistry`:

```go
func newRegistry(cfg *config.Config, mc *metrics.Collector, db *diskstore.DB) *Registry {
    return &Registry{
        cfg:            cfg,
        mc:             mc,
        txidList:       diskstore.NewTxidList(db),
        tokenCallback:  make(map[string]CallbackInfo),
        minerSubtrees:  make([][]*MinerSubtree, cfg.NumMiners),
        stumpStore:     diskstore.NewDiskStumpStore(db),
        txidIndex:      diskstore.NewDiskTxIDIndex(db),
        tokenReg:       stump.NewTokenRegistry(),
        tokenProofs:    make(map[string][]*SubtreeProof),
        blockCh:        make(chan *BlockFinalizedEvent, 1),
        minerSubStore:  diskstore.NewMinerSubtreeStore(db),
    }
}
```

- [ ] **Step 2: Replace allTxids with txidList**

Change the Registry struct:

```go
// Old:
allTxids []chainhash.Hash

// New:
txidList *diskstore.TxidList
```

Update all references:
- `r.allTxids = append(r.allTxids, txid)` → `r.txidList.Append(txid)`
- `len(r.allTxids)` → `r.txidList.Len()`
- `r.allTxids[start:n]` → `r.txidList.Slice(start, n)`
- `r.allTxids[0] = coinbase` → `r.txidList.Set(0, coinbase)`

- [ ] **Step 3: Add minerSubStore field and evict after sealing**

Add to Registry:

```go
minerSubStore *diskstore.MinerSubtreeStore
```

At the end of `sealSubtree`, after persisting to `r.minerSubtrees`, serialize and evict:

```go
// Evict leaf/store arrays to disk for non-current subtrees.
for m := 0; m < cfg.NumMiners; m++ {
    ms := minerSubs[m]
    leavesBytes := hashSliceToBytes(ms.Leaves)
    storeBytes := hashSliceToBytes(ms.Store)
    r.minerSubStore.Save(m, subtreeIdx, leavesBytes, storeBytes)
    // Nil out heavy arrays — Root is retained for discovery.
    ms.Leaves = nil
    ms.Store = nil
}
```

Add helper:

```go
func hashSliceToBytes(hashes []chainhash.Hash) []byte {
    buf := make([]byte, len(hashes)*32)
    for i, h := range hashes {
        copy(buf[i*32:], h[:])
    }
    return buf
}

func bytesToHashSlice(data []byte) []chainhash.Hash {
    n := len(data) / 32
    result := make([]chainhash.Hash, n)
    for i := 0; i < n; i++ {
        copy(result[i][:], data[i*32:])
    }
    return result
}
```

- [ ] **Step 4: Reload subtree-0 from disk during coinbase reseal**

In `finalizeBlock`, before the coinbase reseal loop that accesses `ms.Leaves`, reload from disk:

```go
for m := 0; m < r.cfg.NumMiners; m++ {
    ms := r.minerSubtrees[m][0]
    if ms.Leaves == nil {
        leavesBytes, storeBytes, ok := r.minerSubStore.Load(m, 0)
        if !ok {
            slog.Error("coinbase: failed to reload subtree-0 from disk", "miner", m)
            continue
        }
        ms.Leaves = bytesToHashSlice(leavesBytes)
        ms.Store = bytesToHashSlice(storeBytes)
    }
}
```

- [ ] **Step 5: Run all tests**

```bash
cd /Users/personal/git/STUMPT && go test ./... -v -count=1
```

Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/merkleservice/registry.go internal/merkleservice/types.go
git commit -m "feat: swap registry to disk-backed stores, evict MinerSubtree after sealing"
```

---

### Task 11: End-to-end integration test

**Files:**
- Modify: existing test files or create `internal/merkleservice/integration_test.go`

- [ ] **Step 1: Run the full harness at small scale**

```bash
cd /Users/personal/git/STUMPT && go run ./cmd/harness/ -hashes-per-block 256 -hashes-per-subtree 16 -businesses 10 -duration 5s
```

Expected: completes without errors, BUMPs delivered successfully.

- [ ] **Step 2: Run at larger scale with direct mode**

```bash
cd /Users/personal/git/STUMPT && go run ./cmd/harness/ -direct -hashes-per-block 65536 -hashes-per-subtree 1024 -businesses 100
```

Expected: completes, verify memory stays bounded (not growing linearly).

- [ ] **Step 3: Run benchmarks comparing in-memory vs disk**

```bash
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -bench . -benchmem -count=3
cd /Users/personal/git/STUMPT && go test ./internal/stump/ -bench . -benchmem -count=3
```

Record and compare numbers.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "test: verify disk-backed storage end-to-end at small and large scale"
```

---

## Chunk 4: Performance Tuning

### Task 12: Batch writes for TxIDIndex using WriteBatch

Individual `Update` calls per txid in `DiskTxIDIndex.Set` will be slow at scale. Buffer sets and flush periodically.

**Files:**
- Modify: `internal/diskstore/txid_index.go`

- [ ] **Step 1: Add write batching**

Add a `WriteBatch` method that accepts multiple txid→token pairs and uses `badger.WriteBatch`:

```go
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

type TxIDTokenPair struct {
    TxID  chainhash.Hash
    Token string
}
```

- [ ] **Step 2: Add buffered Set with auto-flush**

Wrap `Set` with an internal buffer that flushes every N entries (e.g., 1000):

```go
// BufferedTxIDIndex wraps DiskTxIDIndex with write buffering.
type BufferedTxIDIndex struct {
    inner   *DiskTxIDIndex
    buf     []TxIDTokenPair
    bufSize int
    mu      sync.Mutex
}

func NewBufferedTxIDIndex(db *DB, bufSize int) *BufferedTxIDIndex {
    return &BufferedTxIDIndex{
        inner:   NewDiskTxIDIndex(db),
        bufSize: bufSize,
    }
}

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

func (b *BufferedTxIDIndex) Flush() {
    b.mu.Lock()
    batch := b.buf
    b.buf = nil
    b.mu.Unlock()
    if len(batch) > 0 {
        b.inner.SetBatch(batch)
    }
}

func (b *BufferedTxIDIndex) Get(txid chainhash.Hash) (string, bool) {
    // Check buffer first.
    b.mu.Lock()
    for _, p := range b.buf {
        if p.TxID == txid {
            b.mu.Unlock()
            return p.Token, true
        }
    }
    b.mu.Unlock()
    return b.inner.Get(txid)
}

func (b *BufferedTxIDIndex) Len() int { return b.inner.Len() }
```

- [ ] **Step 3: Wire BufferedTxIDIndex into registry**

In `newRegistry`, use `diskstore.NewBufferedTxIDIndex(db, 1000)` and add a `Flush()` call in `sealSubtree` before reading back txid→token mappings.

- [ ] **Step 4: Run tests and benchmarks**

```bash
cd /Users/personal/git/STUMPT && go test ./... -v -count=1
cd /Users/personal/git/STUMPT && go test ./internal/diskstore/ -bench BenchmarkDiskTxID -benchmem
```

- [ ] **Step 5: Commit**

```bash
git add internal/diskstore/txid_index.go internal/merkleservice/registry.go
git commit -m "perf: add write-buffered TxIDIndex to batch BadgerDB writes"
```

---

### Task 13: Tune BadgerDB options for write-heavy workload

**Files:**
- Modify: `internal/diskstore/db.go`

- [ ] **Step 1: Optimize BadgerDB settings**

Update the options in `Open`:

```go
opts := badger.DefaultOptions(dir).
    WithLogger(nil).
    WithNumVersionsToKeep(1).
    WithCompactL0OnClose(false).
    WithNumCompactors(2).
    WithValueLogFileSize(256 << 20).     // 256 MB value log files
    WithMemTableSize(64 << 20).          // 64 MB memtable
    WithNumMemtables(2).                 // Keep 2 memtables
    WithBlockCacheSize(0).               // Disable block cache (write-heavy)
    WithIndexCacheSize(100 << 20).       // 100 MB index cache
    WithDetectConflicts(false)           // No transaction conflicts expected
```

- [ ] **Step 2: Run large-scale benchmark**

```bash
cd /Users/personal/git/STUMPT && go run ./cmd/harness/ -direct -hashes-per-block 65536 -hashes-per-subtree 1024 -businesses 100
```

Compare timing with pre-tuning run.

- [ ] **Step 3: Commit**

```bash
git add internal/diskstore/db.go
git commit -m "perf: tune BadgerDB options for write-heavy STUMP workload"
```
