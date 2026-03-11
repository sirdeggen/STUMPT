package subtree_test

// compound_test.go exercises the full two-layer merkle proof structure used by
// the harness:
//
//   Block of N txids split into K subtrees of size S each.
//   Each txid's BUMP has:
//     levels 0…subtreeHeight-1 : sibling hashes within its subtree
//     levels subtreeHeight…blockHeight-1 : sibling hashes in the top tree
//
//   A compound BUMP for a token combines individual BUMPs for all its txids
//   via MerklePath.Combine and must still verify each txid against the block root.

import (
	"crypto/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	gosdk "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// buildBlockBUMP creates a full-block BUMP for txid at globalIdx in a block
// whose subtrees are described by subtreeLeaves.
//
//	subtreeLeaves[s][i] = leaf at local position i in subtree s.
//	subtreeHeight = log2(len(subtreeLeaves[0]))
func buildBlockBUMP(t *testing.T, blockHeight uint32, globalIdx int, subtreeLeaves [][]chainhash.Hash) *gosdk.MerklePath {
	t.Helper()

	s := len(subtreeLeaves[0]) // subtree size
	subtreeIdx := globalIdx / s
	localIdx := globalIdx % s

	subtreeHeight := subtree.Log2Ceil(s)
	numSubtrees := len(subtreeLeaves)
	topTreeHeight := subtree.Log2Ceil(numSubtrees)
	totalHeight := subtreeHeight + topTreeHeight

	// Subtree proof.
	subProof, err := subtree.GetMerkleProof(subtreeLeaves[subtreeIdx], localIdx)
	if err != nil {
		t.Fatalf("subtree proof: %v", err)
	}

	// Subtree roots → top tree.
	subtreeRoots := make([]chainhash.Hash, numSubtrees)
	for i, leaves := range subtreeLeaves {
		subtreeRoots[i] = *subtree.MerkleRoot(leaves)
	}
	topProof, err := subtree.GetMerkleProof(subtreeRoots, subtreeIdx)
	if err != nil {
		t.Fatalf("top tree proof: %v", err)
	}

	g := uint64(globalIdx) //nolint:gosec
	path := make([][]*gosdk.PathElement, totalHeight)
	txid := subtreeLeaves[subtreeIdx][localIdx]

	// Level 0: txid + leaf sibling.
	path[0] = []*gosdk.PathElement{
		{Offset: g, Hash: &txid, Txid: boolPtr(true)},
		{Offset: g ^ 1, Hash: subProof[0]},
	}
	// Subtree levels 1..subtreeHeight-1.
	for k := 1; k < subtreeHeight; k++ {
		path[k] = []*gosdk.PathElement{
			{Offset: (g >> k) ^ 1, Hash: subProof[k]},
		}
	}
	// Top-tree levels.
	for k := 0; k < topTreeHeight; k++ {
		bl := subtreeHeight + k
		path[bl] = []*gosdk.PathElement{
			{Offset: (g >> bl) ^ 1, Hash: topProof[k]},
		}
	}

	return gosdk.NewMerklePath(blockHeight, path)
}

// blockRoot computes the block merkle root from the given subtrees.
func blockRoot(subtreeLeaves [][]chainhash.Hash) *chainhash.Hash {
	roots := make([]chainhash.Hash, len(subtreeLeaves))
	for i, leaves := range subtreeLeaves {
		roots[i] = *subtree.MerkleRoot(leaves)
	}
	return subtree.MerkleRoot(roots)
}

// TestCompoundBUMPAcrossSubtrees builds a small block (4 subtrees × 4 leaves = 16 txids)
// and verifies:
//  1. Each individual txid's BUMP computes to the block root.
//  2. Combining multiple per-txid BUMPs for a "token" produces a compound
//     BUMP that still verifies each txid.
func TestCompoundBUMPAcrossSubtrees(t *testing.T) {
	const (
		numSubtrees = 4
		subtreeSize = 4 // must be power-of-2
		blockHeight = 800_000
	)

	// Generate leaves.
	subtreeLeaves := make([][]chainhash.Hash, numSubtrees)
	for s := range subtreeLeaves {
		subtreeLeaves[s] = make([]chainhash.Hash, subtreeSize)
		for i := range subtreeLeaves[s] {
			if _, err := rand.Read(subtreeLeaves[s][i][:]); err != nil {
				t.Fatal(err)
			}
		}
	}

	root := blockRoot(subtreeLeaves)

	// Verify every individual txid.
	for s := 0; s < numSubtrees; s++ {
		for i := 0; i < subtreeSize; i++ {
			globalIdx := s*subtreeSize + i
			mp := buildBlockBUMP(t, blockHeight, globalIdx, subtreeLeaves)
			txid := subtreeLeaves[s][i]
			got, err := mp.ComputeRoot(&txid)
			if err != nil {
				t.Fatalf("s=%d i=%d ComputeRoot: %v", s, i, err)
			}
			if *got != *root {
				t.Fatalf("s=%d i=%d: root mismatch", s, i)
			}
		}
	}

	// Pick a "token" with txids spread across all subtrees (one per subtree).
	tokenGlobalIndices := []int{2, 5, 10, 13} // one from each subtree

	var compound *gosdk.MerklePath
	for _, gi := range tokenGlobalIndices {
		mp := buildBlockBUMP(t, blockHeight, gi, subtreeLeaves)
		if compound == nil {
			compound = mp
		} else {
			if err := compound.Combine(mp); err != nil {
				t.Fatalf("Combine gi=%d: %v", gi, err)
			}
		}
	}

	// Verify every txid in the compound BUMP.
	for _, gi := range tokenGlobalIndices {
		s := gi / subtreeSize
		i := gi % subtreeSize
		txid := subtreeLeaves[s][i]
		got, err := compound.ComputeRoot(&txid)
		if err != nil {
			t.Fatalf("compound gi=%d ComputeRoot: %v", gi, err)
		}
		if *got != *root {
			t.Fatalf("compound gi=%d: root mismatch", gi)
		}
	}
}

// TestCompoundBUMPDefaultConfig mimics the default harness parameters:
//
//	HashesPerBlock=61440, HashesPerSubtree=1024, NumSubtrees=60
//
// For speed we only test a small random sample of txids, but this validates
// the tree-height arithmetic and proof correctness at scale.
func TestCompoundBUMPDefaultConfig(t *testing.T) {
	const (
		hashesPerBlock   = 61_440
		hashesPerSubtree = 1_024
		numSubtrees      = hashesPerBlock / hashesPerSubtree // 60
		blockHeight      = 800_000
	)

	// Generate leaves. Using a deterministic pattern for reproducibility.
	subtreeLeaves := make([][]chainhash.Hash, numSubtrees)
	for s := range subtreeLeaves {
		subtreeLeaves[s] = make([]chainhash.Hash, hashesPerSubtree)
		for i := range subtreeLeaves[s] {
			// Encode (subtree, local) into the hash bytes for determinism.
			subtreeLeaves[s][i][0] = byte(s)
			subtreeLeaves[s][i][1] = byte(s >> 8)
			subtreeLeaves[s][i][2] = byte(i)
			subtreeLeaves[s][i][3] = byte(i >> 8)
			// Fill rest with pseudo-random but reproducible bytes.
			for j := 4; j < 32; j++ {
				subtreeLeaves[s][i][j] = byte(s*1000 + i*7 + j*13)
			}
		}
	}

	root := blockRoot(subtreeLeaves)

	// Sample: verify 10 txids spread across the block.
	sampleGlobalIdxs := []int{
		0, 1023, 1024, 2047, 30000, 30720, 61000, 61439,
		512, 32768,
	}
	var compound *gosdk.MerklePath
	for _, gi := range sampleGlobalIdxs {
		mp := buildBlockBUMP(t, blockHeight, gi, subtreeLeaves)

		// Verify individually first.
		s := gi / hashesPerSubtree
		i := gi % hashesPerSubtree
		txid := subtreeLeaves[s][i]
		got, err := mp.ComputeRoot(&txid)
		if err != nil {
			t.Fatalf("gi=%d ComputeRoot: %v", gi, err)
		}
		if *got != *root {
			t.Fatalf("gi=%d: root mismatch", gi)
		}

		// Accumulate compound.
		if compound == nil {
			compound = mp
		} else {
			if err := compound.Combine(mp); err != nil {
				t.Fatalf("Combine gi=%d: %v", gi, err)
			}
		}
	}

	// Verify all txids in the compound BUMP.
	for _, gi := range sampleGlobalIdxs {
		s := gi / hashesPerSubtree
		i := gi % hashesPerSubtree
		txid := subtreeLeaves[s][i]
		got, err := compound.ComputeRoot(&txid)
		if err != nil {
			t.Fatalf("compound gi=%d ComputeRoot: %v", gi, err)
		}
		if *got != *root {
			t.Fatalf("compound gi=%d: root mismatch after Combine", gi)
		}
	}
}
