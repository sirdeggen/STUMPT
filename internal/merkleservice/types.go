package merkleservice

import (
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
)

// WatchRequest is the JSON body of POST /watch.
type WatchRequest struct {
	TxID     string       `json:"txid"`
	Callback CallbackInfo `json:"callback"`
}

// CallbackInfo holds the delivery target for a submitting business.
type CallbackInfo struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// SubtreeProof holds a pre-computed miner-0 merkle proof for a single txid.
// The SiblingPath contains sibling hashes at subtree levels 0 … subtreeHeight-1.
// At BUMP-assembly time the top-tree levels are appended from the sealed
// subtree-root list.
type SubtreeProof struct {
	TxID        chainhash.Hash
	SubtreeIdx  int               // which of the block's subtrees this txid lives in
	LocalIdx    int               // position within miner-0's jittered ordering of that subtree
	GlobalIdx   int               // = SubtreeIdx * HashesPerSubtree + LocalIdx  (miner-0 block offset)
	SiblingPath []*chainhash.Hash // len = SubtreeHeight
}

// MinerSubtree is one sealed subtree for one miner.
type MinerSubtree struct {
	Index  int
	Leaves []chainhash.Hash // leaves in this miner's ordering
	Root   chainhash.Hash
	Store  []chainhash.Hash // flat merkle-store (internal nodes only)
}

// BlockFinalizedEvent is sent to the BUMP processor when all txids have arrived.
type BlockFinalizedEvent struct {
	// SubtreeRoots is the ordered list of miner-0 subtree roots.
	SubtreeRoots []chainhash.Hash
	// Callbacks maps each token to its callback delivery target.
	Callbacks map[string]CallbackInfo
	// TokenSubtreeIdx is the lightweight index of token → (subtreeIdx → []localIdx).
	// BUMP assembly uses this to find leaf positions, then computes proofs JIT.
	TokenSubtreeIdx *TokenSubtreeIndex
	// Miner0Subtrees holds the in-memory miner-0 subtree data (leaves + store).
	// BUMP assembly reads directly from these instead of loading from disk.
	Miner0Subtrees []*MinerSubtree
	// MinerSubStore provides disk-backed access to non-miner-0 subtree data.
	MinerSubStore *diskstore.MinerSubtreeStore
	// HashesPerSubtree is needed to compute GlobalIdx from (subtreeIdx, localIdx).
	HashesPerSubtree int
}
