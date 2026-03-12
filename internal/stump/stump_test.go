package stump_test

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func randomHash() chainhash.Hash {
	var h chainhash.Hash
	if _, err := rand.Read(h[:]); err != nil {
		panic(err)
	}
	return h
}

// TestXORKeySymmetry verifies XOR(a, b) == XOR(b, a).
func TestXORKeySymmetry(t *testing.T) {
	a, b := randomHash(), randomHash()
	if stump.XORKey(a, b) != stump.XORKey(b, a) {
		t.Fatal("XOR is not commutative")
	}
}

// TestXORKeySelfInverse verifies XOR(XOR(a, b), b) == a.
func TestXORKeySelfInverse(t *testing.T) {
	a, b := randomHash(), randomHash()
	k := stump.XORKey(a, b)
	recovered := stump.XORKey(k, b)
	if recovered != a {
		t.Fatal("XOR is not self-inverse")
	}
}

// TestXORKeyZero verifies XOR(a, a) == 0.
func TestXORKeyZero(t *testing.T) {
	a := randomHash()
	k := stump.XORKey(a, a)
	var zero chainhash.Hash
	if k != zero {
		t.Fatal("XOR(a, a) should be zero")
	}
}

// TestXORKeyDistinctInputs verifies that different inputs produce different keys.
func TestXORKeyDistinctInputs(t *testing.T) {
	a, b, c := randomHash(), randomHash(), randomHash()
	k1 := stump.XORKey(a, b)
	k2 := stump.XORKey(a, c)
	if k1 == k2 {
		t.Fatal("different inputs should produce different XOR keys (astronomically unlikely collision)")
	}
}

// TestTokenHashDeterministic verifies TokenHash is deterministic.
func TestTokenHashDeterministic(t *testing.T) {
	h1 := stump.TokenHash("token-42")
	h2 := stump.TokenHash("token-42")
	if h1 != h2 {
		t.Fatal("TokenHash should be deterministic")
	}
}

// TestTokenHashDistinct verifies different tokens produce different hashes.
func TestTokenHashDistinct(t *testing.T) {
	h1 := stump.TokenHash("token-0")
	h2 := stump.TokenHash("token-1")
	if h1 == h2 {
		t.Fatal("different tokens should have different hashes")
	}
}

// TestStoreAppendAndGet verifies basic store operations.
func TestStoreAppendAndGet(t *testing.T) {
	store := stump.NewStore()
	key := randomHash()
	e := &stump.Entry{
		TxID:       randomHash(),
		SubtreeIdx: 5,
		LocalIdx:   42,
		GlobalIdx:  5*64 + 42,
	}

	store.Append(key, e)

	got := store.Get(key)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].TxID != e.TxID {
		t.Fatal("entry mismatch")
	}
}

// TestStoreAppendBatch verifies batch append.
func TestStoreAppendBatch(t *testing.T) {
	store := stump.NewStore()
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

// TestStoreMissReturnsNil verifies Get returns nil for unknown keys.
func TestStoreMissReturnsNil(t *testing.T) {
	store := stump.NewStore()
	if got := store.Get(randomHash()); got != nil {
		t.Fatal("expected nil for missing key")
	}
}

// TestTxIDIndex verifies the txid→token reverse index.
func TestTxIDIndex(t *testing.T) {
	idx := stump.NewTxIDIndex(100)
	txid := randomHash()
	idx.Set(txid, "token-7")

	tok, ok := idx.Get(txid)
	if !ok || tok != "token-7" {
		t.Fatalf("expected token-7, got %q (ok=%v)", tok, ok)
	}

	_, ok = idx.Get(randomHash())
	if ok {
		t.Fatal("expected miss for unknown txid")
	}
}

// TestTokenRegistry verifies token registration and hash lookup.
func TestTokenRegistry(t *testing.T) {
	reg := stump.NewTokenRegistry()

	h1 := reg.Register("token-0")
	h2 := reg.Register("token-0") // idempotent
	if h1 != h2 {
		t.Fatal("re-registering should return same hash")
	}

	h3 := reg.Register("token-1")
	if h3 == h1 {
		t.Fatal("different tokens should have different hashes")
	}

	if reg.Len() != 2 {
		t.Fatalf("expected 2 tokens, got %d", reg.Len())
	}

	tokens := reg.Tokens()
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens in snapshot, got %d", len(tokens))
	}
}

// TestDiscoverEndToEnd simulates the full STUMP lifecycle:
// 1. Register tokens and txids
// 2. Seal subtrees and index STUMPs via XOR keys
// 3. Discover matching STUMPs at block announcement
func TestDiscoverEndToEnd(t *testing.T) {
	const (
		numTokens   = 10
		numSubtrees = 4
		txPerToken  = 8 // spread across subtrees
	)

	store := stump.NewStore()
	tokenReg := stump.NewTokenRegistry()
	txidIdx := stump.NewTxIDIndex(numTokens * txPerToken)

	// Register tokens.
	for i := 0; i < numTokens; i++ {
		tokenReg.Register(fmt.Sprintf("token-%d", i))
	}

	// Simulate txid arrival and subtree sealing.
	subtreeRoots := make([]chainhash.Hash, numSubtrees)
	// Track expected: token → list of txids
	expected := make(map[string][]chainhash.Hash)

	for s := 0; s < numSubtrees; s++ {
		subtreeRoots[s] = randomHash() // mock subtree root

		// Each subtree has txPerToken/numSubtrees txids per token (2 each).
		for tok := 0; tok < numTokens; tok++ {
			token := fmt.Sprintf("token-%d", tok)
			th, _ := tokenReg.Hash(token)
			key := stump.XORKey(th, subtreeRoots[s])

			txidsInSubtree := txPerToken / numSubtrees
			entries := make([]*stump.Entry, txidsInSubtree)
			for i := 0; i < txidsInSubtree; i++ {
				txid := randomHash()
				txidIdx.Set(txid, token)
				entries[i] = &stump.Entry{
					TxID:       txid,
					SubtreeIdx: s,
					LocalIdx:   tok*txidsInSubtree + i,
					GlobalIdx:  s*64 + tok*txidsInSubtree + i,
				}
				expected[token] = append(expected[token], txid)
			}
			store.AppendBatch(key, entries)
		}
	}

	// Discover at block announcement.
	discovered := stump.Discover(store, tokenReg, subtreeRoots)

	// Verify each token found all its txids.
	for tok := 0; tok < numTokens; tok++ {
		token := fmt.Sprintf("token-%d", tok)
		entries := discovered[token]
		if len(entries) != txPerToken {
			t.Fatalf("token %s: expected %d entries, got %d", token, txPerToken, len(entries))
		}

		// Verify all expected txids are present.
		foundTxids := make(map[chainhash.Hash]bool)
		for _, e := range entries {
			foundTxids[e.TxID] = true
		}
		for _, expTxid := range expected[token] {
			if !foundTxids[expTxid] {
				t.Fatalf("token %s: missing expected txid", token)
			}
		}
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkXORKey measures the cost of a single XOR operation.
func BenchmarkXORKey(b *testing.B) {
	a, bb := randomHash(), randomHash()
	b.ResetTimer()
	for range b.N {
		_ = stump.XORKey(a, bb)
	}
}

// BenchmarkTokenHash measures the cost of hashing a token string.
func BenchmarkTokenHash(b *testing.B) {
	b.ResetTimer()
	for range b.N {
		_ = stump.TokenHash("token-12345")
	}
}

// BenchmarkStoreAppend measures append throughput under no contention.
func BenchmarkStoreAppend(b *testing.B) {
	store := stump.NewStore()
	key := randomHash()
	e := &stump.Entry{TxID: randomHash()}
	b.ResetTimer()
	for range b.N {
		store.Append(key, e)
	}
}

// BenchmarkStoreGet measures lookup throughput.
func BenchmarkStoreGet(b *testing.B) {
	store := stump.NewStore()
	key := randomHash()
	store.Append(key, &stump.Entry{TxID: randomHash()})
	b.ResetTimer()
	for range b.N {
		_ = store.Get(key)
	}
}

// BenchmarkDiscover100Subtrees100Tokens measures Discover at moderate scale.
func BenchmarkDiscover100Subtrees100Tokens(b *testing.B) {
	store := stump.NewStore()
	reg := stump.NewTokenRegistry()
	roots := make([]chainhash.Hash, 100)

	for i := 0; i < 100; i++ {
		reg.Register(fmt.Sprintf("token-%d", i))
		roots[i] = randomHash()
	}

	// Populate: each token has entries in every subtree.
	for _, root := range roots {
		for i := 0; i < 100; i++ {
			th, _ := reg.Hash(fmt.Sprintf("token-%d", i))
			key := stump.XORKey(th, root)
			store.Append(key, &stump.Entry{TxID: randomHash()})
		}
	}

	b.ResetTimer()
	for range b.N {
		_ = stump.Discover(store, reg, roots)
	}
}

// BenchmarkDiscover6000Subtrees1000Tokens measures Discover at larger scale.
func BenchmarkDiscover6000Subtrees1000Tokens(b *testing.B) {
	store := stump.NewStore()
	reg := stump.NewTokenRegistry()
	roots := make([]chainhash.Hash, 6000)

	for i := 0; i < 1000; i++ {
		reg.Register(fmt.Sprintf("token-%d", i))
	}
	for i := range roots {
		roots[i] = randomHash()
	}

	// Populate: each token has entries in every subtree.
	for _, root := range roots {
		for i := 0; i < 1000; i++ {
			th, _ := reg.Hash(fmt.Sprintf("token-%d", i))
			key := stump.XORKey(th, root)
			store.Append(key, &stump.Entry{TxID: randomHash()})
		}
	}

	b.ResetTimer()
	for range b.N {
		_ = stump.Discover(store, reg, roots)
	}
}
