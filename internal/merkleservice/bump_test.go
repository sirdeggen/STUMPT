package merkleservice

import (
	"crypto/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

func randomLeaves(n int) []chainhash.Hash {
	leaves := make([]chainhash.Hash, n)
	for i := range leaves {
		if _, err := rand.Read(leaves[i][:]); err != nil {
			panic(err)
		}
	}
	return leaves
}

// setupBUMPBenchmark creates the data structures needed for buildCompoundBUMP
// benchmarking: numSubtrees subtrees of subtreeSize leaves each, with
// proofsPerToken proofs spread across the subtrees.
func setupBUMPBenchmark(numSubtrees, subtreeSize, proofsPerToken int) (
	proofs []*SubtreeProof,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
) {
	subtreeHeight = subtree.Log2Ceil(subtreeSize)
	topTreeHeight = subtree.Log2Ceil(numSubtrees)
	totalHeight = subtreeHeight + topTreeHeight

	// Build subtrees and collect roots.
	subtreeLeaves := make([][]chainhash.Hash, numSubtrees)
	subtreeStores := make([][]chainhash.Hash, numSubtrees)
	roots := make([]chainhash.Hash, numSubtrees)

	for s := 0; s < numSubtrees; s++ {
		subtreeLeaves[s] = randomLeaves(subtreeSize)
		subtreeStores[s] = subtree.BuildMerkleStore(subtreeLeaves[s])
		roots[s] = subtreeStores[s][len(subtreeStores[s])-1]
	}

	// Build top tree proofs.
	topProofs, err := subtree.GetAllProofs(roots)
	if err != nil {
		panic(err)
	}

	// Distribute proofs across subtrees.
	proofs = make([]*SubtreeProof, 0, proofsPerToken)
	proofsPerSubtree := proofsPerToken / numSubtrees
	if proofsPerSubtree < 1 {
		proofsPerSubtree = 1
	}

	for s := 0; s < numSubtrees && len(proofs) < proofsPerToken; s++ {
		for i := 0; i < proofsPerSubtree && len(proofs) < proofsPerToken; i++ {
			localIdx := i % subtreeSize
			sp, err := subtree.GetProofFromStore(subtreeLeaves[s], subtreeStores[s], localIdx)
			if err != nil {
				panic(err)
			}
			proofs = append(proofs, &SubtreeProof{
				TxID:        subtreeLeaves[s][localIdx],
				SubtreeIdx:  s,
				LocalIdx:    localIdx,
				GlobalIdx:   s*subtreeSize + localIdx,
				SiblingPath: sp,
			})
		}
	}

	return
}

// BenchmarkBuildCompoundBUMP_10proofs_16subtrees benchmarks compound BUMP
// assembly for a small token (10 txids, 16 subtrees × 64 leaves = default config).
func BenchmarkBuildCompoundBUMP_10proofs_16subtrees(b *testing.B) {
	proofs, topProofs, sh, tth, th := setupBUMPBenchmark(16, 64, 10)
	b.ResetTimer()
	for range b.N {
		_, _ = buildCompoundBUMP(800_000, proofs, topProofs, sh, tth, th)
	}
}

// BenchmarkBuildCompoundBUMP_600proofs_60subtrees benchmarks at 100 tx/s
// scale (60 subtrees × 1024, 600 txids/token).
func BenchmarkBuildCompoundBUMP_600proofs_60subtrees(b *testing.B) {
	proofs, topProofs, sh, tth, th := setupBUMPBenchmark(60, 1024, 600)
	b.ResetTimer()
	for range b.N {
		_, _ = buildCompoundBUMP(800_000, proofs, topProofs, sh, tth, th)
	}
}

// BenchmarkBuildCompoundBUMP_6000proofs_600subtrees benchmarks at 1k tx/s
// scale (600 subtrees × 1000, 6000 txids/token).
func BenchmarkBuildCompoundBUMP_6000proofs_600subtrees(b *testing.B) {
	proofs, topProofs, sh, tth, th := setupBUMPBenchmark(600, 1000, 6000)
	b.ResetTimer()
	for range b.N {
		_, _ = buildCompoundBUMP(800_000, proofs, topProofs, sh, tth, th)
	}
}

// BenchmarkBuildCompoundBUMP_60000proofs_6000subtrees benchmarks at 10k tx/s
// scale (6000 subtrees × 1000, 60000 txids/token for 100 businesses).
func BenchmarkBuildCompoundBUMP_60000proofs_6000subtrees(b *testing.B) {
	proofs, topProofs, sh, tth, th := setupBUMPBenchmark(6000, 1000, 60000)
	b.ResetTimer()
	for range b.N {
		_, _ = buildCompoundBUMP(800_000, proofs, topProofs, sh, tth, th)
	}
}
