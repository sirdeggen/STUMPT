package diskstore

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestMinerSubtreeStoreRoundTrip(t *testing.T) {
	db := openTestDB(t)
	s := NewMinerSubtreeStore(db)

	// Generate random leaves and store data.
	leaves := make([]byte, 128)
	if _, err := rand.Read(leaves); err != nil {
		t.Fatal(err)
	}
	store := make([]byte, 256)
	if _, err := rand.Read(store); err != nil {
		t.Fatal(err)
	}

	// Save miner=0, subtree=3.
	if err := s.Save(0, 3, leaves, store); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load back and verify.
	gotLeaves, gotStore, ok := s.Load(0, 3)
	if !ok {
		t.Fatal("Load returned false for saved key")
	}
	if !bytes.Equal(gotLeaves, leaves) {
		t.Error("leaves mismatch")
	}
	if !bytes.Equal(gotStore, store) {
		t.Error("store mismatch")
	}

	// Different miner should not be found.
	_, _, ok = s.Load(1, 3)
	if ok {
		t.Error("Load returned true for non-existent miner=1 subtree=3")
	}
}
