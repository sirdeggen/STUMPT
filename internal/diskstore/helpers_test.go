package diskstore

import (
	"crypto/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randomHash(t testing.TB) chainhash.Hash {
	t.Helper()
	var h chainhash.Hash
	if _, err := rand.Read(h[:]); err != nil {
		t.Fatal(err)
	}
	return h
}
