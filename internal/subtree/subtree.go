// Package subtree provides merkle tree construction and proof generation using
// go-sdk's chainhash types.  It intentionally does NOT depend on go-subtree or
// go-bt; the hashing algorithm is identical to transaction.MerkleTreeParent in
// go-sdk so the resulting proofs are wire-compatible with go-sdk's MerklePath /
// BUMP format.
package subtree

import (
	"crypto/sha256"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// ── hashing ──────────────────────────────────────────────────────────────────

// HashPair computes SHA256d(left || right), which is identical to
// transaction.MerkleTreeParent in go-sdk.  Both inputs are raw (non-reversed)
// chainhash.Hash values.
func HashPair(l, r chainhash.Hash) chainhash.Hash {
	var buf [64]byte
	copy(buf[:32], l[:])
	copy(buf[32:], r[:])
	h1 := sha256.Sum256(buf[:])
	return chainhash.Hash(sha256.Sum256(h1[:]))
}

// ── helpers ───────────────────────────────────────────────────────────────────

// NextPowerOfTwo returns the smallest power of 2 that is ≥ n.
func NextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// Log2Ceil returns ⌈log₂(n)⌉ — the smallest h such that 2^h ≥ n.
func Log2Ceil(n int) int {
	h := 0
	for (1 << h) < n {
		h++
	}
	return h
}

// padLeaf returns leaves[i] when i < n, else duplicates the last leaf (Bitcoin
// convention for odd-count levels).
func padLeaf(leaves []chainhash.Hash, i, n int) chainhash.Hash {
	if i >= n {
		return leaves[n-1]
	}
	return leaves[i]
}

// ── BuildMerkleStore ──────────────────────────────────────────────────────────

// BuildMerkleStore computes all internal (non-leaf) nodes of the merkle tree
// for the given leaf hashes and returns them in a flat slice.
//
// Layout for pad=8 (N ≤ 8 leaves):
//
//	indices 0-3  : level-1 parents  (hash of pairs of leaves)
//	indices 4-5  : level-2 parents
//	index  6     : root
//
// The root is always store[len(store)-1].
// For N=1 the function returns []Hash{leaves[0]}.
func BuildMerkleStore(leaves []chainhash.Hash) []chainhash.Hash {
	n := len(leaves)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return []chainhash.Hash{leaves[0]}
	}

	pad := NextPowerOfTwo(n)
	store := make([]chainhash.Hash, pad-1)

	// Level 1: hash adjacent pairs of leaves.
	for i := 0; i < pad; i += 2 {
		l := padLeaf(leaves, i, n)
		r := padLeaf(leaves, i+1, n)
		store[i/2] = HashPair(l, r)
	}

	// Build each subsequent level from the previous one.
	offset := 0     // start index of the current level in store
	size := pad / 2 // number of nodes at the current level
	for size > 1 {
		nextSize := size / 2
		for i := 0; i < size; i += 2 {
			store[offset+size+i/2] = HashPair(store[offset+i], store[offset+i+1])
		}
		offset += size
		size = nextSize
	}

	return store
}

// MerkleRoot returns the merkle root of the given leaves.
func MerkleRoot(leaves []chainhash.Hash) *chainhash.Hash {
	if len(leaves) == 0 {
		return nil
	}
	store := BuildMerkleStore(leaves)
	root := store[len(store)-1]
	return &root
}

// ── GetMerkleProof ────────────────────────────────────────────────────────────

// GetMerkleProof returns the sibling-hash path needed to prove that leaves[index]
// is part of the merkle tree.
//
//   - proof[0]  = sibling at the leaf level (block merkle level 0)
//   - proof[k]  = sibling at block merkle level k
//   - len(proof) = Log2Ceil(len(leaves))
//
// The proof is compatible with go-sdk's MerklePath / BUMP format: at BUMP level k
// the sibling's offset is (globalLeafOffset >> k) ^ 1.
func GetMerkleProof(leaves []chainhash.Hash, index int) ([]*chainhash.Hash, error) {
	n := len(leaves)
	if index < 0 || index >= n {
		return nil, fmt.Errorf("subtree: index %d out of range [0, %d)", index, n)
	}
	if n == 1 {
		// Single-leaf tree: the root IS the leaf; no sibling proof is needed.
		return []*chainhash.Hash{}, nil
	}

	height := Log2Ceil(n)
	store := BuildMerkleStore(leaves)
	return proofFromStore(leaves, store, index, height, n), nil
}

// GetProofFromStore computes the proof for leaf at index using a pre-built
// store.  Avoids rebuilding the merkle store when calling for many leaves.
func GetProofFromStore(leaves []chainhash.Hash, store []chainhash.Hash, index int) ([]*chainhash.Hash, error) {
	n := len(leaves)
	if index < 0 || index >= n {
		return nil, fmt.Errorf("subtree: index %d out of range [0, %d)", index, n)
	}
	height := Log2Ceil(n)
	return proofFromStore(leaves, store, index, height, n), nil
}

// proofFromStore builds the sibling list from a pre-built store.
func proofFromStore(leaves, store []chainhash.Hash, index, height, n int) []*chainhash.Hash {
	proof := make([]*chainhash.Hash, height)

	// Level 0: leaf sibling.
	sibIdx := index ^ 1
	if sibIdx >= n {
		sibIdx = index // duplicate last leaf (Bitcoin convention)
	}
	h := leaves[sibIdx]
	proof[0] = &h

	// Levels 1..height-1: walk up the store.
	pad := NextPowerOfTwo(n)
	offset := 0     // start of current level in store
	size := pad / 2 // nodes at current level
	for level := 1; level < height; level++ {
		sibPos := (index >> level) ^ 1
		// Guard: sibPos should always be in range for a power-of-2-padded tree,
		// but clamp defensively.
		if sibPos >= size {
			sibPos = size - 1
		}
		h := store[offset+sibPos]
		proof[level] = &h
		offset += size
		size /= 2
	}

	return proof
}

// ── GetAllProofs ──────────────────────────────────────────────────────────────

// GetAllProofs builds the merkle store once and returns proofs for every leaf.
// This is more efficient than calling GetMerkleProof N times.
func GetAllProofs(leaves []chainhash.Hash) ([][]*chainhash.Hash, error) {
	n := len(leaves)
	if n == 0 {
		return nil, nil
	}
	height := Log2Ceil(n)
	store := BuildMerkleStore(leaves)
	proofs := make([][]*chainhash.Hash, n)
	for i := 0; i < n; i++ {
		proofs[i] = proofFromStore(leaves, store, i, height, n)
	}
	return proofs, nil
}
