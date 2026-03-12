package diskstore_test

import (
	"crypto/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func openStumpTestDB(t *testing.T) *diskstore.DB {
	t.Helper()
	db, err := diskstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randomHash() chainhash.Hash {
	var h chainhash.Hash
	rand.Read(h[:])
	return h
}

func makeEntry(subtreeIdx, localIdx, globalIdx int, numSiblings int) *stump.Entry {
	e := &stump.Entry{
		TxID:       randomHash(),
		SubtreeIdx: subtreeIdx,
		LocalIdx:   localIdx,
		GlobalIdx:  globalIdx,
	}
	e.SiblingPath = make([]*chainhash.Hash, numSiblings)
	for i := range e.SiblingPath {
		h := randomHash()
		e.SiblingPath[i] = &h
	}
	return e
}

func TestDiskStoreAppendAndGet(t *testing.T) {
	db := openStumpTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key := randomHash()
	entry := makeEntry(1, 2, 3, 4)

	store.Append(key, entry)

	got := store.Get(key)
	if got == nil {
		t.Fatal("expected entries, got nil")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}

	g := got[0]
	if g.TxID != entry.TxID {
		t.Errorf("TxID mismatch: got %x, want %x", g.TxID, entry.TxID)
	}
	if g.SubtreeIdx != entry.SubtreeIdx {
		t.Errorf("SubtreeIdx: got %d, want %d", g.SubtreeIdx, entry.SubtreeIdx)
	}
	if g.LocalIdx != entry.LocalIdx {
		t.Errorf("LocalIdx: got %d, want %d", g.LocalIdx, entry.LocalIdx)
	}
	if g.GlobalIdx != entry.GlobalIdx {
		t.Errorf("GlobalIdx: got %d, want %d", g.GlobalIdx, entry.GlobalIdx)
	}
	if len(g.SiblingPath) != len(entry.SiblingPath) {
		t.Fatalf("SiblingPath length: got %d, want %d", len(g.SiblingPath), len(entry.SiblingPath))
	}
	for i := range g.SiblingPath {
		if *g.SiblingPath[i] != *entry.SiblingPath[i] {
			t.Errorf("SiblingPath[%d]: got %x, want %x", i, *g.SiblingPath[i], *entry.SiblingPath[i])
		}
	}
}

func TestDiskStoreAppendBatch(t *testing.T) {
	db := openStumpTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key := randomHash()
	entries := make([]*stump.Entry, 10)
	for i := range entries {
		entries[i] = makeEntry(i, i*2, i*3, 2)
	}

	store.AppendBatch(key, entries)

	got := store.Get(key)
	if len(got) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(got))
	}
}

func TestDiskStoreMissReturnsNil(t *testing.T) {
	db := openStumpTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	got := store.Get(randomHash())
	if got != nil {
		t.Fatalf("expected nil for unknown key, got %d entries", len(got))
	}
}

func TestDiskStoreMultipleAppendsAccumulate(t *testing.T) {
	db := openStumpTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key := randomHash()
	for i := 0; i < 3; i++ {
		store.Append(key, makeEntry(i, i, i, 1))
	}

	got := store.Get(key)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
}

func TestDiskStoreLen(t *testing.T) {
	db := openStumpTestDB(t)
	store := diskstore.NewDiskStumpStore(db)

	key1 := randomHash()
	key2 := randomHash()

	store.Append(key1, makeEntry(0, 0, 0, 1))
	store.Append(key1, makeEntry(1, 1, 1, 1))
	store.Append(key2, makeEntry(2, 2, 2, 1))

	if n := store.Len(); n != 2 {
		t.Fatalf("expected Len()=2, got %d", n)
	}
}

func TestDiskStoreInterfaceCompliance(t *testing.T) {
	db := openStumpTestDB(t)
	var _ stump.StumpStore = diskstore.NewDiskStumpStore(db)
}

func BenchmarkDiskStoreAppend(b *testing.B) {
	db, err := diskstore.Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	store := diskstore.NewDiskStumpStore(db)
	key := randomHash()
	entry := makeEntry(1, 2, 3, 4)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Append(key, entry)
	}
}

func BenchmarkDiskStoreGet(b *testing.B) {
	db, err := diskstore.Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	store := diskstore.NewDiskStumpStore(db)
	key := randomHash()
	for i := 0; i < 100; i++ {
		store.Append(key, makeEntry(i, i, i, 2))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Get(key)
	}
}
