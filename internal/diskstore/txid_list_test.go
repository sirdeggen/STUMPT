package diskstore

import (
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

func TestTxidListAppendAndGet(t *testing.T) {
	db := openTestDB(t)
	tl := NewTxidList(db)

	hashes := [3]chainhash.Hash{
		randomHash(t),
		randomHash(t),
		randomHash(t),
	}

	for _, h := range hashes {
		if err := tl.Append(h); err != nil {
			t.Fatal(err)
		}
	}

	if tl.Len() != 3 {
		t.Fatalf("expected Len==3, got %d", tl.Len())
	}

	got0, ok := tl.Get(0)
	if !ok {
		t.Fatal("Get(0) returned false")
	}
	if got0 != hashes[0] {
		t.Errorf("Get(0) mismatch")
	}

	got2, ok := tl.Get(2)
	if !ok {
		t.Fatal("Get(2) returned false")
	}
	if got2 != hashes[2] {
		t.Errorf("Get(2) mismatch")
	}

	_, ok = tl.Get(99)
	if ok {
		t.Error("Get(99) should return false")
	}
}

func TestTxidListSet(t *testing.T) {
	db := openTestDB(t)
	tl := NewTxidList(db)

	original := randomHash(t)
	if err := tl.Append(original); err != nil {
		t.Fatal(err)
	}

	replacement := randomHash(t)
	if err := tl.Set(0, replacement); err != nil {
		t.Fatal(err)
	}

	got, ok := tl.Get(0)
	if !ok {
		t.Fatal("Get(0) returned false after Set")
	}
	if got != replacement {
		t.Errorf("Get(0) should return replacement hash")
	}
	if got == original {
		t.Errorf("Get(0) still returns original hash")
	}
}

func TestTxidListSlice(t *testing.T) {
	db := openTestDB(t)
	tl := NewTxidList(db)

	hashes := make([]chainhash.Hash, 10)
	for i := range hashes {
		hashes[i] = randomHash(t)
		if err := tl.Append(hashes[i]); err != nil {
			t.Fatal(err)
		}
	}

	got := tl.Slice(3, 7)
	if len(got) != 4 {
		t.Fatalf("expected 4 elements, got %d", len(got))
	}
	for i, h := range got {
		if h != hashes[3+i] {
			t.Errorf("Slice element %d mismatch", i)
		}
	}
}

func BenchmarkTxidListAppend(b *testing.B) {
	db, err := Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	tl := NewTxidList(db)
	var h chainhash.Hash

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Use index as pseudo-hash content to avoid crypto/rand overhead.
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		h[2] = byte(i >> 16)
		h[3] = byte(i >> 24)
		if err := tl.Append(h); err != nil {
			b.Fatal(err)
		}
	}
}
