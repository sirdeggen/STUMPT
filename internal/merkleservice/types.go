package merkleservice

import (
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
)

// CallbackInfo holds the delivery target for a submitting business.
type CallbackInfo struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// SubtreeProof holds a merkle proof for a single txid within the winning miner's
// subtree ordering. The SiblingPath contains sibling hashes at subtree levels
// 0 … subtreeHeight-1. At BUMP-assembly time the top-tree levels are appended
// from the sealed subtree-root list.
type SubtreeProof struct {
	TxID        chainhash.Hash
	SubtreeIdx  int               // which of the block's subtrees this txid lives in
	LocalIdx    int               // position within the winning miner's ordering of that subtree
	GlobalIdx   int               // = SubtreeIdx * HashesPerSubtree + LocalIdx
	SiblingPath []*chainhash.Hash // len = SubtreeHeight
}

// MinerSubtree is one sealed subtree for one miner.
type MinerSubtree struct {
	Index  int
	Leaves []chainhash.Hash // leaves in this miner's ordering
	Root   chainhash.Hash
	Store  []chainhash.Hash // flat merkle-store (internal nodes only)
}

// BlockFinalizedEvent is produced at "block found" time and drives BUMP assembly.
type BlockFinalizedEvent struct {
	// WinnerMiner is the index of the miner that won the block (0-based).
	WinnerMiner int
	// SubtreeRoots is the ordered list of the winning miner's subtree roots.
	SubtreeRoots []chainhash.Hash
	// TokenSubtreeIdx is the winning miner's token→(subtreeIdx→[]localIdx) index.
	// BUMP assembly uses this to find leaf positions, then computes proofs JIT.
	TokenSubtreeIdx *TokenSubtreeIndex
	// MinerSubStore provides disk-backed loading of subtree data.
	MinerSubStore *diskstore.MinerSubtreeStore
	// HashesPerSubtree is needed to compute GlobalIdx from (subtreeIdx, localIdx).
	HashesPerSubtree int
	// NumBusinesses is the total number of business tokens.
	NumBusinesses int
}
