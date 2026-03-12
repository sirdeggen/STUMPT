package merkleservice

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// Registry holds all mutable state for the Merkle Service.
type Registry struct {
	mu  sync.Mutex
	cfg *config.Config
	mc  *metrics.Collector

	// Global ordered list of every received txid.
	allTxids []chainhash.Hash

	// per-token accumulated txids and callback targets.
	tokenTxids    map[string][]chainhash.Hash
	tokenCallback map[string]CallbackInfo

	// minerSubtrees[minerIdx] is the ordered list of sealed subtrees for that miner.
	minerSubtrees [][]*MinerSubtree

	// tokenProofs[token] is the growing list of miner-0 SubtreeProofs accumulated
	// as subtrees are sealed incrementally.
	tokenProofs map[string][]*SubtreeProof

	// blockCh is closed once the block is complete, signalling the server.
	blockCh chan *BlockFinalizedEvent
}

// newRegistry creates an initialised Registry.
func newRegistry(cfg *config.Config, mc *metrics.Collector) *Registry {
	return &Registry{
		cfg:           cfg,
		mc:            mc,
		allTxids:      make([]chainhash.Hash, 0, cfg.HashesPerBlock),
		tokenTxids:    make(map[string][]chainhash.Hash),
		tokenCallback: make(map[string]CallbackInfo),
		minerSubtrees: make([][]*MinerSubtree, cfg.NumMiners),
		tokenProofs:   make(map[string][]*SubtreeProof),
		blockCh:       make(chan *BlockFinalizedEvent, 1),
	}
}

// AddTxID records a new txid, triggers subtree sealing when appropriate, and
// returns a BlockFinalizedEvent (non-nil) when the block is now complete.
//
// Subtree sealing is synchronous (blocking the HTTP handler for ~30 ms once
// every ~10 s at the default rate) so that the pre-computed proofs are always
// ready before the next subtree starts.
func (r *Registry) AddTxID(txidHex, token string, cb CallbackInfo) *BlockFinalizedEvent {
	txid, err := hexToHash(txidHex)
	if err != nil {
		slog.Warn("registry: bad txid hex", "err", err)
		return nil
	}

	r.mu.Lock()
	r.allTxids = append(r.allTxids, txid)
	r.tokenTxids[token] = append(r.tokenTxids[token], txid)
	r.tokenCallback[token] = cb

	n := len(r.allTxids)
	cfg := r.cfg

	// Seal a subtree every HashesPerSubtree txids.
	if n%cfg.HashesPerSubtree == 0 {
		subtreeIdx := (n / cfg.HashesPerSubtree) - 1
		start := subtreeIdx * cfg.HashesPerSubtree
		base := make([]chainhash.Hash, cfg.HashesPerSubtree)
		copy(base, r.allTxids[start:n])
		r.mu.Unlock()

		r.sealSubtree(subtreeIdx, base)
	} else {
		r.mu.Unlock()
	}

	// Check for block completion after lock is released.
	r.mu.Lock()
	n2 := len(r.allTxids)
	r.mu.Unlock()

	if n2 == cfg.HashesPerBlock {
		return r.finalizeBlock()
	}
	return nil
}

// sealSubtree builds all miners' versions of the subtree and pre-computes
// miner-0 proofs.  Called without the registry lock.
func (r *Registry) sealSubtree(subtreeIdx int, baseTxids []chainhash.Hash) {
	cfg := r.cfg
	t0 := time.Now()

	minerSubs := make([]*MinerSubtree, cfg.NumMiners)
	for m := 0; m < cfg.NumMiners; m++ {
		leaves := jitterTxids(baseTxids, m, subtreeIdx, cfg.JitterPercent)
		store := subtree.BuildMerkleStore(leaves)
		root := store[len(store)-1]
		minerSubs[m] = &MinerSubtree{
			Index:  subtreeIdx,
			Leaves: leaves,
			Root:   root,
			Store:  store,
		}
	}

	r.mc.RecordSubtreeSeal(time.Since(t0))

	slog.Info("subtree sealed",
		"subtreeIdx", subtreeIdx,
		"miners", cfg.NumMiners,
		"sealDuration", time.Since(t0),
	)

	// Persist sealed subtrees.
	r.mu.Lock()
	for m := 0; m < cfg.NumMiners; m++ {
		r.minerSubtrees[m] = append(r.minerSubtrees[m], minerSubs[m])
	}
	r.mu.Unlock()

	// Pre-compute miner-0 proofs for every token with txids in this subtree.
	t1 := time.Now()
	miner0 := minerSubs[0]

	// Build a reverse-index: txid -> local position in miner-0's ordering.
	localIdx := make(map[chainhash.Hash]int, cfg.HashesPerSubtree)
	for i, h := range miner0.Leaves {
		localIdx[h] = i
	}

	// Mark which txids belong to this subtree (canonical set).
	inSubtree := make(map[chainhash.Hash]struct{}, cfg.HashesPerSubtree)
	for _, h := range baseTxids {
		inSubtree[h] = struct{}{}
	}

	// Build the proof list for each token that has at least one txid here.
	r.mu.Lock()
	tokSnap := make(map[string][]chainhash.Hash, len(r.tokenTxids))
	for tok, txids := range r.tokenTxids {
		tokSnap[tok] = txids
	}
	r.mu.Unlock()

	start := subtreeIdx * cfg.HashesPerSubtree
	newProofs := make(map[string][]*SubtreeProof)

	for tok, txids := range tokSnap {
		for _, txid := range txids {
			if _, ok := inSubtree[txid]; !ok {
				continue
			}
			li, ok := localIdx[txid]
			if !ok {
				continue
			}
			sp, err := subtree.GetProofFromStore(miner0.Leaves, miner0.Store, li)
			if err != nil {
				slog.Error("proof generation failed", "err", err)
				continue
			}
			txidCopy := txid
			newProofs[tok] = append(newProofs[tok], &SubtreeProof{
				TxID:        txidCopy,
				SubtreeIdx:  subtreeIdx,
				LocalIdx:    li,
				GlobalIdx:   start + li,
				SiblingPath: sp,
			})
		}
	}

	// Merge into global proof map.
	r.mu.Lock()
	for tok, proofs := range newProofs {
		r.tokenProofs[tok] = append(r.tokenProofs[tok], proofs...)
	}
	r.mu.Unlock()

	r.mc.RecordProofCompute(time.Since(t1))

	slog.Info("proofs pre-computed",
		"subtreeIdx", subtreeIdx,
		"tokens", len(newProofs),
		"proofDuration", time.Since(t1),
	)
}

// finalizeBlock simulates block discovery: replaces the coinbase placeholder
// in subtree-0, re-seals it, recomputes affected proofs, then extracts the
// data needed to build and deliver BUMPs.
func (r *Registry) finalizeBlock() *BlockFinalizedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	slog.Info("block complete", "txids", len(r.allTxids))

	// ── Coinbase replacement ────────────────────────────────────────────────
	// At block discovery the real coinbase txid becomes known.  Simulate this
	// by generating a random 32-byte hash and swapping it into position 0 of
	// subtree-0 for every miner.
	t0 := time.Now()

	var coinbase chainhash.Hash
	if _, err := rand.Read(coinbase[:]); err != nil {
		slog.Error("coinbase: random generation failed", "err", err)
	}
	oldCoinbase := r.allTxids[0]
	r.allTxids[0] = coinbase

	slog.Info("coinbase replaced",
		"old", hex.EncodeToString(oldCoinbase[:8]),
		"new", hex.EncodeToString(coinbase[:8]),
	)

	// Re-seal subtree-0 for every miner: replace leaf-0, rebuild store+root.
	for m := 0; m < r.cfg.NumMiners; m++ {
		ms := r.minerSubtrees[m][0]
		// Find where the old coinbase lives in this miner's leaf ordering.
		// For miner-0 it's always index 0 (canonical order).
		// For jittered miners it could have moved.
		replaced := false
		for i, h := range ms.Leaves {
			if h == oldCoinbase {
				ms.Leaves[i] = coinbase
				replaced = true
				break
			}
		}
		if !replaced {
			slog.Error("coinbase: old coinbase not found in miner subtree",
				"miner", m)
		}
		ms.Store = subtree.BuildMerkleStore(ms.Leaves)
		ms.Root = ms.Store[len(ms.Store)-1]
	}

	// Recompute miner-0 proofs for all tokens that had txids in subtree-0.
	// The sibling paths changed because the coinbase changed the left spine
	// of the subtree-0 merkle tree.
	miner0 := r.minerSubtrees[0][0]
	localIdx := make(map[chainhash.Hash]int, r.cfg.HashesPerSubtree)
	for i, h := range miner0.Leaves {
		localIdx[h] = i
	}

	recomputedProofs := 0
	for tok, proofList := range r.tokenProofs {
		for i, p := range proofList {
			if p.SubtreeIdx != 0 {
				continue
			}
			// The txid itself may have changed (if this was the coinbase slot).
			txid := p.TxID
			if txid == oldCoinbase {
				txid = coinbase
			}
			li, ok := localIdx[txid]
			if !ok {
				slog.Error("coinbase reseal: txid not found in miner-0 subtree-0",
					"token", tok, "txid", hex.EncodeToString(txid[:8]))
				continue
			}
			sp, err := subtree.GetProofFromStore(miner0.Leaves, miner0.Store, li)
			if err != nil {
				slog.Error("coinbase reseal: proof recomputation failed",
					"token", tok, "err", err)
				continue
			}
			r.tokenProofs[tok][i] = &SubtreeProof{
				TxID:        txid,
				SubtreeIdx:  0,
				LocalIdx:    li,
				GlobalIdx:   li,
				SiblingPath: sp,
			}
			recomputedProofs++
		}
	}

	coinbaseDur := time.Since(t0)
	r.mc.RecordCoinbaseReseal(coinbaseDur)
	slog.Info("coinbase reseal complete",
		"duration", coinbaseDur,
		"recomputedProofs", recomputedProofs,
	)

	// ── Build event ─────────────────────────────────────────────────────────
	m0 := r.minerSubtrees[0]
	roots := make([]chainhash.Hash, len(m0))
	for i, ms := range m0 {
		roots[i] = ms.Root
	}

	cbs := make(map[string]CallbackInfo, len(r.tokenCallback))
	for k, v := range r.tokenCallback {
		cbs[k] = v
	}
	proofs := make(map[string][]*SubtreeProof, len(r.tokenProofs))
	for k, v := range r.tokenProofs {
		cp := make([]*SubtreeProof, len(v))
		copy(cp, v)
		proofs[k] = cp
	}

	return &BlockFinalizedEvent{
		SubtreeRoots: roots,
		TokenProofs:  proofs,
		Callbacks:    cbs,
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// hexToHash converts a raw-bytes hex string to chainhash.Hash (no byte reversal).
func hexToHash(s string) (chainhash.Hash, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return chainhash.Hash{}, err
	}
	if len(b) != chainhash.HashSize {
		return chainhash.Hash{}, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	var h chainhash.Hash
	copy(h[:], b)
	return h, nil
}

// jitterTxids returns a copy of txids with a fraction of adjacent pairs swapped.
// Miner 0 always receives the canonical order (jitterFrac[0] == 0).
// The RNG seed is deterministic per (miner, subtree) pair.
func jitterTxids(txids []chainhash.Hash, minerIdx, subtreeIdx int, jitterFrac []float64) []chainhash.Hash {
	result := make([]chainhash.Hash, len(txids))
	copy(result, txids)

	if minerIdx >= len(jitterFrac) || jitterFrac[minerIdx] == 0.0 || len(result) < 2 {
		return result
	}

	//nolint:gosec // deterministic seed is intentional for reproducibility
	rng := mathrand.New(mathrand.NewSource(int64(minerIdx*1_000_000 + subtreeIdx)))
	numSwaps := int(float64(len(result)) * jitterFrac[minerIdx])
	for i := 0; i < numSwaps; i++ {
		pos := rng.Intn(len(result) - 1)
		result[pos], result[pos+1] = result[pos+1], result[pos]
	}
	return result
}
