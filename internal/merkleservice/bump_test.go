package merkleservice

import (
	"bytes"
	"crypto/rand"
	"fmt"
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

// setupFastBenchmark creates data structures for assembleTokenBUMPFast testing.
// Returns positions, subtreeMap, topProofs, and tree dimensions.
func setupFastBenchmark(numSubtrees, subtreeSize, proofsPerToken int) (
	positions []leafPos,
	subtreeMap map[int]*cachedSubtree,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
) {
	subtreeHeight = subtree.Log2Ceil(subtreeSize)
	topTreeHeight = subtree.Log2Ceil(numSubtrees)
	totalHeight = subtreeHeight + topTreeHeight

	subtreeLeaves := make([][]chainhash.Hash, numSubtrees)
	subtreeStores := make([][]chainhash.Hash, numSubtrees)
	roots := make([]chainhash.Hash, numSubtrees)

	for s := 0; s < numSubtrees; s++ {
		subtreeLeaves[s] = randomLeaves(subtreeSize)
		subtreeStores[s] = subtree.BuildMerkleStore(subtreeLeaves[s])
		roots[s] = subtreeStores[s][len(subtreeStores[s])-1]
	}

	topProofs, err := subtree.GetAllProofs(roots)
	if err != nil {
		panic(err)
	}

	// Build subtreeMap and positions.
	subtreeMap = make(map[int]*cachedSubtree, numSubtrees)
	for s := 0; s < numSubtrees; s++ {
		subtreeMap[s] = &cachedSubtree{
			Leaves: subtreeLeaves[s],
			Store:  subtreeStores[s],
		}
	}

	proofsPerSubtree := proofsPerToken / numSubtrees
	if proofsPerSubtree < 1 {
		proofsPerSubtree = 1
	}

	positions = make([]leafPos, 0, proofsPerToken)
	for s := 0; s < numSubtrees && len(positions) < proofsPerToken; s++ {
		for i := 0; i < proofsPerSubtree && len(positions) < proofsPerToken; i++ {
			localIdx := i % subtreeSize
			positions = append(positions, leafPos{
				subtreeIdx: s,
				localIdx:   localIdx,
				globalIdx:  s*subtreeSize + localIdx,
			})
		}
	}

	return
}

// TestFastVsLegacyBUMP verifies that assembleTokenBUMPFast produces identical
// BUMP binary output to assembleTokenBUMP + MerklePath.Bytes().
func TestFastVsLegacyBUMP(t *testing.T) {
	tests := []struct {
		name       string
		numST      int
		stSize     int
		numProofs  int
	}{
		{"4st_16leaf_8proof", 4, 16, 8},
		{"8st_64leaf_40proof", 8, 64, 40},
		{"16st_128leaf_100proof", 16, 128, 100},
		{"4st_256leaf_200proof", 4, 256, 200},
		{"2st_32leaf_4proof", 2, 32, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			positions, subtreeMap, topProofs, sh, tth, th := setupFastBenchmark(tc.numST, tc.stSize, tc.numProofs)

			// Legacy path: assembleTokenBUMP + Bytes().
			legacyBump, err := assembleTokenBUMP(
				800_000, positions, subtreeMap, topProofs, sh, tth, th,
			)
			if err != nil {
				t.Fatalf("legacy assembleTokenBUMP failed: %v", err)
			}
			legacyBytes := legacyBump.Bytes()

			// Fast path: assembleTokenBUMPFast (returns bytes directly).
			ws := newWorkerState(th)
			fastBytes, err := assembleTokenBUMPFast(
				800_000, positions, subtreeMap, topProofs, sh, tth, th, ws,
			)
			if err != nil {
				t.Fatalf("fast assembleTokenBUMPFast failed: %v", err)
			}

			if !bytes.Equal(legacyBytes, fastBytes) {
				t.Errorf("BUMP bytes differ: legacy=%d bytes, fast=%d bytes", len(legacyBytes), len(fastBytes))
				// Show first divergence point.
				minLen := len(legacyBytes)
				if len(fastBytes) < minLen {
					minLen = len(fastBytes)
				}
				for i := 0; i < minLen; i++ {
					if legacyBytes[i] != fastBytes[i] {
						t.Errorf("first diff at byte %d: legacy=0x%02x fast=0x%02x", i, legacyBytes[i], fastBytes[i])
						break
					}
				}
			}
		})
	}
}

// TestFragmentVsJIT verifies that the fragment-based path produces
// byte-identical BUMP output to the JIT path (assembleTokenBUMPFast).
func TestFragmentVsJIT(t *testing.T) {
	tests := []struct {
		name      string
		numST     int
		stSize    int
		numProofs int
	}{
		{"4st_16leaf_8proof", 4, 16, 8},
		{"8st_64leaf_40proof", 8, 64, 40},
		{"16st_128leaf_100proof", 16, 128, 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			positions, subtreeMap, topProofs, sh, tth, th := setupFastBenchmark(tc.numST, tc.stSize, tc.numProofs)

			// JIT path.
			wsJIT := newWorkerState(th)
			jitBytes, err := assembleTokenBUMPFast(
				800_000, positions, subtreeMap, topProofs, sh, tth, th, wsJIT,
			)
			if err != nil {
				t.Fatalf("JIT failed: %v", err)
			}

			// Fragment path: extract fragments per subtree, then assemble.
			wsExtract := newWorkerState(sh)

			// Group positions by subtree to extract fragments.
			subtreeLocalIdxs := make(map[int][]int32)
			for _, pos := range positions {
				subtreeLocalIdxs[pos.subtreeIdx] = append(
					subtreeLocalIdxs[pos.subtreeIdx], int32(pos.localIdx),
				)
			}

			var fragmentData [][]byte
			var subtreeIdxList []int
			for si := 0; si < tc.numST; si++ {
				idxs, ok := subtreeLocalIdxs[si]
				if !ok {
					continue
				}
				sc := subtreeMap[si]
				levels := extractSubtreeFragment(
					sc.Leaves, sc.Store, idxs,
					si, tc.stSize, sh, wsExtract,
				)
				fragmentData = append(fragmentData, MarshalFragment(levels))
				subtreeIdxList = append(subtreeIdxList, si)
			}

			wsFrag := newWorkerState(th)
			fragBytes, err := assembleTokenBUMPFromFragments(
				800_000, fragmentData, subtreeIdxList, topProofs,
				sh, tth, th, tc.stSize, wsFrag,
			)
			if err != nil {
				t.Fatalf("fragment path failed: %v", err)
			}

			if !bytes.Equal(jitBytes, fragBytes) {
				t.Errorf("BUMP bytes differ: JIT=%d bytes, fragment=%d bytes", len(jitBytes), len(fragBytes))
				minLen := len(jitBytes)
				if len(fragBytes) < minLen {
					minLen = len(fragBytes)
				}
				for i := 0; i < minLen; i++ {
					if jitBytes[i] != fragBytes[i] {
						t.Errorf("first diff at byte %d: JIT=0x%02x frag=0x%02x", i, jitBytes[i], fragBytes[i])
						break
					}
				}
			}
		})
	}
}

// BenchmarkAssembleTokenBUMPFast benchmarks the new fast path.
func BenchmarkAssembleTokenBUMPFast(b *testing.B) {
	configs := []struct {
		name  string
		numST int
		stSz  int
		nProof int
	}{
		{"10proof_16st", 16, 64, 10},
		{"600proof_60st", 60, 1024, 600},
		{"6000proof_600st", 600, 1000, 6000},
	}
	for _, c := range configs {
		b.Run(fmt.Sprintf("fast_%s", c.name), func(b *testing.B) {
			pos, stMap, tp, sh, tth, th := setupFastBenchmark(c.numST, c.stSz, c.nProof)
			ws := newWorkerState(th)
			b.ResetTimer()
			for range b.N {
				_, _ = assembleTokenBUMPFast(800_000, pos, stMap, tp, sh, tth, th, ws)
			}
		})
	}
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
