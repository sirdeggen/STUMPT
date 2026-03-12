package diskstore

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func TestDiskTxIDIndexSetGet(t *testing.T) {
	db := openTestDB(t)
	idx := NewDiskTxIDIndex(db)

	txid := randomHash(t)
	idx.Set(txid, "token-7")

	got, ok := idx.Get(txid)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != "token-7" {
		t.Fatalf("expected token-7, got %s", got)
	}
}

func TestDiskTxIDIndexMiss(t *testing.T) {
	db := openTestDB(t)
	idx := NewDiskTxIDIndex(db)

	txid := randomHash(t)
	got, ok := idx.Get(txid)
	if ok {
		t.Fatal("expected ok=false for missing txid")
	}
	if got != "" {
		t.Fatalf("expected empty string, got %s", got)
	}
}

func TestDiskTxIDIndexInterfaceCompliance(t *testing.T) {
	db := openTestDB(t)
	var _ stump.TxIDIndexer = NewDiskTxIDIndex(db)
}

func TestDiskTxIDIndexLen(t *testing.T) {
	db := openTestDB(t)
	idx := NewDiskTxIDIndex(db)

	if idx.Len() != 0 {
		t.Fatalf("expected 0, got %d", idx.Len())
	}

	for i := 0; i < 5; i++ {
		idx.Set(randomHash(t), "tok")
	}
	if idx.Len() != 5 {
		t.Fatalf("expected 5, got %d", idx.Len())
	}
}

func TestBufferedTxIDIndex(t *testing.T) {
	db := openTestDB(t)
	idx := NewBufferedTxIDIndex(db, 5) // flush every 5

	// Write 3 (under threshold) — should be in buffer.
	for i := 0; i < 3; i++ {
		idx.Set(randomHash(t), fmt.Sprintf("tok-%d", i))
	}
	// Get from buffer should work.
	txid := randomHash(t)
	idx.Set(txid, "buffered-token")
	tok, ok := idx.Get(txid)
	if !ok || tok != "buffered-token" {
		t.Fatalf("expected buffered-token, got %q ok=%v", tok, ok)
	}

	// Write enough to trigger flush (1 more to reach bufSize=5).
	idx.Set(randomHash(t), "tok-flush")

	// Manual flush for remaining.
	idx.Flush()
}

func TestBufferedTxIDIndexInterfaceCompliance(t *testing.T) {
	db := openTestDB(t)
	var _ stump.TxIDIndexer = NewBufferedTxIDIndex(db, 10)
}

func BenchmarkDiskTxIDIndexSet(b *testing.B) {
	db, err := Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	idx := NewDiskTxIDIndex(db)
	hashes := make([]chainhash.Hash, b.N)
	for i := range hashes {
		if _, err := rand.Read(hashes[i][:]); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.Set(hashes[i], "bench-token")
	}
}

func BenchmarkDiskTxIDIndexGet(b *testing.B) {
	db, err := Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	idx := NewDiskTxIDIndex(db)

	// Pre-populate
	var h chainhash.Hash
	if _, err := rand.Read(h[:]); err != nil {
		b.Fatal(err)
	}
	idx.Set(h, "bench-token")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx.Get(h)
	}
}
