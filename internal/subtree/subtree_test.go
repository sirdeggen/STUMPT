package subtree_test

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	gosdk "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// randomLeaves generates n random 32-byte leaves.
func randomLeaves(t *testing.T, n int) []chainhash.Hash {
	t.Helper()
	leaves := make([]chainhash.Hash, n)
	for i := range leaves {
		if _, err := rand.Read(leaves[i][:]); err != nil {
			t.Fatal(err)
		}
	}
	return leaves
}

// ── Merkle root consistency ───────────────────────────────────────────────────

// TestHashPairMatchesGoSDK verifies that HashPair produces the same result as
// transaction.MerkleTreeParent.
func TestHashPairMatchesGoSDK(t *testing.T) {
	var l, r chainhash.Hash
	_, _ = rand.Read(l[:])
	_, _ = rand.Read(r[:])

	got := subtree.HashPair(l, r)
	want := gosdk.MerkleTreeParent(&l, &r)

	if got != *want {
		t.Fatalf("HashPair mismatch:\n  got  %s\n  want %s",
			hex.EncodeToString(got[:]), hex.EncodeToString(want[:]))
	}
}

// TestMerkleRootSingleLeaf checks the Bitcoin convention: 1-tx block → root = txid.
func TestMerkleRootSingleLeaf(t *testing.T) {
	leaves := randomLeaves(t, 1)
	root := subtree.MerkleRoot(leaves)
	if *root != leaves[0] {
		t.Fatal("single-leaf root should equal the leaf itself")
	}
}

// TestMerkleRootTwoLeaves checks root = hash(l0, l1) for two leaves.
func TestMerkleRootTwoLeaves(t *testing.T) {
	leaves := randomLeaves(t, 2)
	root := subtree.MerkleRoot(leaves)
	want := subtree.HashPair(leaves[0], leaves[1])
	if *root != want {
		t.Fatal("two-leaf root mismatch")
	}
}

// ── Proof round-trip against go-sdk ComputeRoot ──────────────────────────────

// proofToMerklePath converts a subtree proof into a go-sdk MerklePath so we
// can call ComputeRoot on it.
//
// For a single-leaf tree (n=1) the proof is empty and the BUMP contains just
// the txid at level 0; go-sdk's ComputeRoot handles this special case.
func proofToMerklePath(blockHeight uint32, leaves []chainhash.Hash, index int) (*gosdk.MerklePath, error) {
	proof, err := subtree.GetMerkleProof(leaves, index)
	if err != nil {
		return nil, err
	}

	txid := leaves[index]
	g := uint64(index) //nolint:gosec

	if len(proof) == 0 {
		// Single-leaf block: BUMP has one level with just the txid.
		return gosdk.NewMerklePath(blockHeight, [][]*gosdk.PathElement{
			{{Offset: g, Hash: &txid, Txid: boolPtr(true)}},
		}), nil
	}

	height := subtree.Log2Ceil(len(leaves))
	path := make([][]*gosdk.PathElement, height)

	path[0] = []*gosdk.PathElement{
		{Offset: g, Hash: &txid, Txid: boolPtr(true)},
		{Offset: g ^ 1, Hash: proof[0]},
	}
	for k := 1; k < height; k++ {
		path[k] = []*gosdk.PathElement{
			{Offset: (g >> k) ^ 1, Hash: proof[k]},
		}
	}

	return gosdk.NewMerklePath(blockHeight, path), nil
}

func boolPtr(b bool) *bool { return &b }

// TestProofRoundTripSmall exercises 4-leaf trees: each leaf's proof must
// produce the same root via go-sdk ComputeRoot.
func TestProofRoundTripSmall(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 7, 8} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			leaves := randomLeaves(t, n)
			root := subtree.MerkleRoot(leaves)

			for i := 0; i < n; i++ {
				mp, err := proofToMerklePath(800_000, leaves, i)
				if err != nil {
					t.Fatalf("leaf %d: proof build: %v", i, err)
				}
				txid := leaves[i]
				got, err := mp.ComputeRoot(&txid)
				if err != nil {
					t.Fatalf("leaf %d: ComputeRoot: %v", i, err)
				}
				if *got != *root {
					t.Fatalf("leaf %d: root mismatch\n  got  %s\n  want %s",
						i,
						hex.EncodeToString(got[:]),
						hex.EncodeToString(root[:]),
					)
				}
			}
		})
	}
}

// TestProofRoundTrip1024 exercises a full subtree of 1024 leaves — the default
// HashesPerSubtree — sampling every 64th leaf.
func TestProofRoundTrip1024(t *testing.T) {
	leaves := randomLeaves(t, 1024)
	root := subtree.MerkleRoot(leaves)

	for i := 0; i < 1024; i += 64 {
		mp, err := proofToMerklePath(800_000, leaves, i)
		if err != nil {
			t.Fatalf("leaf %d: %v", i, err)
		}
		txid := leaves[i]
		got, err := mp.ComputeRoot(&txid)
		if err != nil {
			t.Fatalf("leaf %d ComputeRoot: %v", i, err)
		}
		if *got != *root {
			t.Fatalf("leaf %d: root mismatch", i)
		}
	}
}

// TestCompoundBUMPCombine verifies that two per-txid BUMPs from the same tree
// can be combined and that ComputeRoot still works for each txid.
func TestCompoundBUMPCombine(t *testing.T) {
	n := 8
	leaves := randomLeaves(t, n)
	root := subtree.MerkleRoot(leaves)

	// Build BUMPs for leaves 0 and 5.
	mp0, err := proofToMerklePath(800_000, leaves, 0)
	if err != nil {
		t.Fatal(err)
	}
	mp5, err := proofToMerklePath(800_000, leaves, 5)
	if err != nil {
		t.Fatal(err)
	}

	if err := mp0.Combine(mp5); err != nil {
		t.Fatalf("Combine: %v", err)
	}

	// Both txids must still verify against the same root.
	for _, idx := range []int{0, 5} {
		txid := leaves[idx]
		got, err := mp0.ComputeRoot(&txid)
		if err != nil {
			t.Fatalf("leaf %d ComputeRoot after Combine: %v", idx, err)
		}
		if *got != *root {
			t.Fatalf("leaf %d: root mismatch after Combine", idx)
		}
	}
}

// ── GetAllProofs ──────────────────────────────────────────────────────────────

func TestGetAllProofs(t *testing.T) {
	leaves := randomLeaves(t, 16)
	root := subtree.MerkleRoot(leaves)

	allProofs, err := subtree.GetAllProofs(leaves)
	if err != nil {
		t.Fatal(err)
	}
	if len(allProofs) != len(leaves) {
		t.Fatalf("want %d proofs, got %d", len(leaves), len(allProofs))
	}

	for i, proof := range allProofs {
		height := subtree.Log2Ceil(len(leaves))
		g := uint64(i)
		path := make([][]*gosdk.PathElement, height)
		txid := leaves[i]
		path[0] = []*gosdk.PathElement{
			{Offset: g, Hash: &txid, Txid: boolPtr(true)},
			{Offset: g ^ 1, Hash: proof[0]},
		}
		for k := 1; k < height; k++ {
			path[k] = []*gosdk.PathElement{{Offset: (g >> k) ^ 1, Hash: proof[k]}}
		}
		mp := gosdk.NewMerklePath(800_000, path)
		got, err := mp.ComputeRoot(&txid)
		if err != nil {
			t.Fatalf("leaf %d ComputeRoot: %v", i, err)
		}
		if *got != *root {
			t.Fatalf("leaf %d root mismatch", i)
		}
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkBuildMerkleStore1024 measures BuildMerkleStore at the default
// subtree size (1024 leaves = HashesPerSubtree).
func BenchmarkBuildMerkleStore1024(b *testing.B) {
	leaves := make([]chainhash.Hash, 1024)
	for i := range leaves {
		leaves[i][0] = byte(i)
		leaves[i][1] = byte(i >> 8)
	}
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

// BenchmarkGetAllProofs1024 measures GetAllProofs at the default subtree size.
func BenchmarkGetAllProofs1024(b *testing.B) {
	leaves := make([]chainhash.Hash, 1024)
	for i := range leaves {
		leaves[i][0] = byte(i)
		leaves[i][1] = byte(i >> 8)
	}
	b.ResetTimer()
	for range b.N {
		_, _ = subtree.GetAllProofs(leaves)
	}
}

// BenchmarkBuildMerkleStore10k measures BuildMerkleStore at 10k leaves.
func BenchmarkBuildMerkleStore10k(b *testing.B) {
	leaves := makeLeaves(10_000)
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

// BenchmarkBuildMerkleStore100k measures BuildMerkleStore at 100k leaves.
func BenchmarkBuildMerkleStore100k(b *testing.B) {
	leaves := makeLeaves(100_000)
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

// BenchmarkGetAllProofs10k measures GetAllProofs at 10k leaves.
func BenchmarkGetAllProofs10k(b *testing.B) {
	leaves := makeLeaves(10_000)
	b.ResetTimer()
	for range b.N {
		_, _ = subtree.GetAllProofs(leaves)
	}
}

// BenchmarkGetAllProofs100k measures GetAllProofs at 100k leaves.
func BenchmarkGetAllProofs100k(b *testing.B) {
	leaves := makeLeaves(100_000)
	b.ResetTimer()
	for range b.N {
		_, _ = subtree.GetAllProofs(leaves)
	}
}

// BenchmarkBuildMerkleStoreFullBlock measures the top-level tree for a full
// 61440-txid block (60 subtrees padded to 64, so 64 subtree-roots as leaves).
func BenchmarkBuildMerkleStoreFullBlock(b *testing.B) {
	leaves := makeLeaves(64)
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

// BenchmarkBuildMerkleStoreTopTree600k measures the top tree for 600k txids
// (600 subtree roots padded to 1024).
func BenchmarkBuildMerkleStoreTopTree600k(b *testing.B) {
	leaves := makeLeaves(1024) // 600 subtrees padded to next power of 2
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

// BenchmarkBuildMerkleStoreTopTree6M measures the top tree for 6M txids
// (60k subtree roots padded to 65536).
func BenchmarkBuildMerkleStoreTopTree6M(b *testing.B) {
	leaves := makeLeaves(65_536)
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

// BenchmarkBuildMerkleStoreTopTree600M measures the top tree for 600M txids
// (600k subtree roots padded to 1M).
func BenchmarkBuildMerkleStoreTopTree600M(b *testing.B) {
	leaves := makeLeaves(1_048_576)
	b.ResetTimer()
	for range b.N {
		_ = subtree.BuildMerkleStore(leaves)
	}
}

func makeLeaves(n int) []chainhash.Hash {
	leaves := make([]chainhash.Hash, n)
	for i := range leaves {
		leaves[i][0] = byte(i)
		leaves[i][1] = byte(i >> 8)
		leaves[i][2] = byte(i >> 16)
	}
	return leaves
}

// itoa is a minimal int-to-string helper to avoid importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
