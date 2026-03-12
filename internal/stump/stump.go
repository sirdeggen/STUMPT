// Package stump implements the STUMP (Subtree-Token Unified Merkle Proof)
// indexing scheme using XOR-based content-addressed keys.
//
// # Motivation
//
// At scale (600M txids/block, 100k+ businesses), the naive approach of iterating
// every token's txid list against every sealed subtree is O(tokens × txids/token)
// per subtree — prohibitively expensive. STUMP replaces this with:
//
//  1. A global txid→token reverse index (O(1) per txid lookup)
//  2. An XOR-indexed STUMP store keyed by XOR(tokenHash, subtreeRoot)
//
// # XOR Key Design
//
// The composite key XOR(tokenHash, subtreeRoot) is chosen because:
//
//   - **Commutativity matters for discovery:** On block announcement, a subscriber
//     who knows their tokenHash can XOR it with each announced subtreeRoot to
//     probe the STUMP store — without iterating all tokens or all subtrees.
//   - **Uniform distribution:** Both tokenHash (SHA256d of the token string) and
//     subtreeRoot (merkle root of random txids) are uniformly distributed 256-bit
//     values, so their XOR is also uniform — no clustering in the hash map.
//   - **Reversibility:** Given the XOR key and either operand, the other is
//     recoverable: subtreeRoot = key ^ tokenHash. This enables verification.
//   - **No hash collision risk:** The key space is 2^256; collisions are
//     astronomically improbable even at 600M txids × 100k tokens.
//
// # Why not concatenation + hash?
//
// Concatenation (e.g. SHA256(token || subtreeRoot)) would work for indexing but
// loses the commutativity property needed for efficient discovery at block time.
// With XOR, a subscriber can probe the store in O(subtrees) rather than
// O(subtrees × tokens).
//
// # Usage in the block lifecycle
//
// Inter-block (subtree sealing):
//
//	For each txid in the sealed subtree:
//	  token := txidIndex.Lookup(txid)
//	  key   := XOR(tokenHash, subtreeRoot)
//	  store.Append(key, proof)
//
// At block announcement:
//
//	For each subtreeRoot in the winning miner's block:
//	  For each subscribed tokenHash:
//	    key := XOR(tokenHash, subtreeRoot)
//	    stumps := store.Get(key)  // O(1) lookup
//	    → append to this token's proof list for BUMP assembly
package stump

import (
	"crypto/sha256"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// Key is a 256-bit XOR composite key: XOR(tokenHash, subtreeRoot).
type Key = chainhash.Hash

// XORKey computes XOR(a, b) for two 256-bit hashes.
// This is the core indexing operation: given a tokenHash and a subtreeRoot,
// the STUMP is stored and retrieved at XOR(tokenHash, subtreeRoot).
func XORKey(a, b chainhash.Hash) Key {
	var k Key
	// Unrolled in 8-byte chunks for performance on 64-bit architectures.
	// chainhash.Hash is [32]byte, so 4 iterations of 8 bytes each.
	for i := 0; i < 32; i += 8 {
		k[i+0] = a[i+0] ^ b[i+0]
		k[i+1] = a[i+1] ^ b[i+1]
		k[i+2] = a[i+2] ^ b[i+2]
		k[i+3] = a[i+3] ^ b[i+3]
		k[i+4] = a[i+4] ^ b[i+4]
		k[i+5] = a[i+5] ^ b[i+5]
		k[i+6] = a[i+6] ^ b[i+6]
		k[i+7] = a[i+7] ^ b[i+7]
	}
	return k
}

// TokenHash computes a deterministic 256-bit hash for a token string.
// Uses SHA256d (double SHA256) to match the Bitcoin convention used everywhere
// else in the merkle engine.
func TokenHash(token string) chainhash.Hash {
	h1 := sha256.Sum256([]byte(token))
	return chainhash.Hash(sha256.Sum256(h1[:]))
}

// Entry is a single proof entry stored under a STUMP key.
// It records one txid's subtree-level proof along with its position metadata.
type Entry struct {
	TxID        chainhash.Hash
	SubtreeIdx  int
	LocalIdx    int
	GlobalIdx   int
	SiblingPath []*chainhash.Hash
}

// Store is a concurrent-safe XOR-indexed STUMP store.
// The map key is XOR(tokenHash, subtreeRoot) and the value is a slice of
// proof entries for all txids belonging to that (token, subtree) pair.
type Store struct {
	mu      sync.RWMutex
	entries map[Key][]*Entry
}

// NewStore creates an empty STUMP store.
func NewStore() *Store {
	return &Store{
		entries: make(map[Key][]*Entry),
	}
}

// Append adds a proof entry under the given XOR key.
// Thread-safe; called from subtree-sealing goroutines.
func (s *Store) Append(key Key, e *Entry) {
	s.mu.Lock()
	s.entries[key] = append(s.entries[key], e)
	s.mu.Unlock()
}

// AppendBatch adds multiple proof entries under the given XOR key in one lock.
func (s *Store) AppendBatch(key Key, entries []*Entry) {
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	s.entries[key] = append(s.entries[key], entries...)
	s.mu.Unlock()
}

// Get returns all proof entries stored under the given XOR key.
// Returns nil if no entries exist. Thread-safe.
func (s *Store) Get(key Key) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[key]
}

// Len returns the total number of XOR keys in the store.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// TxIDIndex maps txid → token string for O(1) reverse lookup.
// Populated as txids arrive via /watch.
type TxIDIndex struct {
	mu     sync.RWMutex
	lookup map[chainhash.Hash]string
}

// NewTxIDIndex creates an empty txid→token index.
func NewTxIDIndex(capacity int) *TxIDIndex {
	return &TxIDIndex{
		lookup: make(map[chainhash.Hash]string, capacity),
	}
}

// Set records that txid belongs to the given token.
func (idx *TxIDIndex) Set(txid chainhash.Hash, token string) {
	idx.mu.Lock()
	idx.lookup[txid] = token
	idx.mu.Unlock()
}

// Get returns the token that owns this txid, or ("", false) if not found.
func (idx *TxIDIndex) Get(txid chainhash.Hash) (string, bool) {
	idx.mu.RLock()
	tok, ok := idx.lookup[txid]
	idx.mu.RUnlock()
	return tok, ok
}

// Len returns the number of indexed txids.
func (idx *TxIDIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.lookup)
}

// TokenRegistry manages the set of known tokens and their pre-computed hashes.
type TokenRegistry struct {
	mu     sync.RWMutex
	hashes map[string]chainhash.Hash // token string → TokenHash
	tokens []string                  // ordered list for iteration
}

// NewTokenRegistry creates an empty token registry.
func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{
		hashes: make(map[string]chainhash.Hash),
	}
}

// Register adds a token (if not already known) and returns its hash.
func (tr *TokenRegistry) Register(token string) chainhash.Hash {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if h, ok := tr.hashes[token]; ok {
		return h
	}
	h := TokenHash(token)
	tr.hashes[token] = h
	tr.tokens = append(tr.tokens, token)
	return h
}

// Hash returns the pre-computed hash for a known token.
func (tr *TokenRegistry) Hash(token string) (chainhash.Hash, bool) {
	tr.mu.RLock()
	h, ok := tr.hashes[token]
	tr.mu.RUnlock()
	return h, ok
}

// Tokens returns a snapshot of all registered token strings.
func (tr *TokenRegistry) Tokens() []string {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	out := make([]string, len(tr.tokens))
	copy(out, tr.tokens)
	return out
}

// Len returns the number of registered tokens.
func (tr *TokenRegistry) Len() int {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	return len(tr.hashes)
}

// Discover performs the block-announcement XOR probe: for each subtreeRoot,
// XOR it with each registered tokenHash and return the matching STUMP entries
// grouped by token.
//
// This is the key performance operation: O(subtrees × tokens) XOR operations
// + O(1) map lookups per probe. At 6000 subtrees × 100k tokens = 600M XOR ops,
// each XOR is ~2ns on modern hardware = ~1.2s. For smaller scales this is
// sub-millisecond.
func Discover(store *Store, registry *TokenRegistry, subtreeRoots []chainhash.Hash) map[string][]*Entry {
	tokens := registry.Tokens()
	result := make(map[string][]*Entry, len(tokens))

	for _, root := range subtreeRoots {
		for _, token := range tokens {
			th, _ := registry.Hash(token)
			key := XORKey(th, root)
			entries := store.Get(key)
			if len(entries) > 0 {
				result[token] = append(result[token], entries...)
			}
		}
	}

	return result
}
