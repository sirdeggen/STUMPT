package merkleservice

import "sync"

// TokenSubtreeIndex is a lightweight in-memory index mapping (token, subtreeIdx)
// to the list of local leaf positions within that subtree.
// This replaces the full stump store for BUMP assembly — proofs are recomputed
// on demand from the merkle store rather than stored.
//
// Memory per entry: ~4 bytes (int32 local index) vs ~860 bytes (full Entry with sibling path).
// At 600M txids this is ~2.4GB vs ~516GB.
type TokenSubtreeIndex struct {
	mu sync.RWMutex
	// entries[token][subtreeIdx] = list of local leaf indices
	entries map[string]map[int][]int32
}

// NewTokenSubtreeIndex creates an empty index.
func NewTokenSubtreeIndex() *TokenSubtreeIndex {
	return &TokenSubtreeIndex{
		entries: make(map[string]map[int][]int32),
	}
}

// Add records that the given token has a txid at localIdx in subtreeIdx.
func (idx *TokenSubtreeIndex) Add(token string, subtreeIdx int, localIdx int) {
	idx.mu.Lock()
	sm := idx.entries[token]
	if sm == nil {
		sm = make(map[int][]int32)
		idx.entries[token] = sm
	}
	sm[subtreeIdx] = append(sm[subtreeIdx], int32(localIdx))
	idx.mu.Unlock()
}

// AddBatch records multiple local indices for one token in one subtree.
func (idx *TokenSubtreeIndex) AddBatch(token string, subtreeIdx int, localIdxs []int32) {
	if len(localIdxs) == 0 {
		return
	}
	idx.mu.Lock()
	sm := idx.entries[token]
	if sm == nil {
		sm = make(map[int][]int32)
		idx.entries[token] = sm
	}
	sm[subtreeIdx] = append(sm[subtreeIdx], localIdxs...)
	idx.mu.Unlock()
}

// Get returns the local indices for a given token and subtreeIdx.
func (idx *TokenSubtreeIndex) Get(token string, subtreeIdx int) []int32 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	sm := idx.entries[token]
	if sm == nil {
		return nil
	}
	return sm[subtreeIdx]
}

// Tokens returns all tokens that have at least one entry.
func (idx *TokenSubtreeIndex) Tokens() []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	tokens := make([]string, 0, len(idx.entries))
	for t := range idx.entries {
		tokens = append(tokens, t)
	}
	return tokens
}

// SubtreeIndices returns which subtrees have entries for the given token.
func (idx *TokenSubtreeIndex) SubtreeIndices(token string) []int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	sm := idx.entries[token]
	if sm == nil {
		return nil
	}
	result := make([]int, 0, len(sm))
	for si := range sm {
		result = append(result, si)
	}
	return result
}
