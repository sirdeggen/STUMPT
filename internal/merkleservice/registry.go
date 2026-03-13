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
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
	"github.com/bsv-blockchain/stumpt/internal/stump"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// Registry holds all mutable state for the Merkle Service.
type Registry struct {
	mu  sync.Mutex
	cfg *config.Config
	mc  *metrics.Collector

	// Disk-backed ordered list of every received txid (buffered writes).
	txidList *diskstore.BufferedTxidList

	// per-token callback targets.
	tokenCallback map[string]CallbackInfo

	// minerSubtrees[minerIdx] is the ordered list of sealed subtrees for that miner.
	// Only Root is retained in memory; Leaves/Store are evicted to disk after sealing.
	minerSubtrees [][]*MinerSubtree

	// ── STUMP indexing ──────────────────────────────────────────────────
	// XOR-indexed STUMP store: key = XOR(tokenHash, subtreeRoot).
	stumpStore stump.StumpStore
	txidIndex  stump.TxIDIndexer
	tokenReg   *stump.TokenRegistry

	// blockCh is closed once the block is complete, signalling the server.
	blockCh chan *BlockFinalizedEvent

	// minerSubStore persists MinerSubtree leaves/store to disk for eviction.
	minerSubStore *diskstore.MinerSubtreeStore
}

// newRegistry creates an initialised Registry.
func newRegistry(cfg *config.Config, mc *metrics.Collector, db *diskstore.DB) *Registry {
	return &Registry{
		cfg:           cfg,
		mc:            mc,
		txidList:      diskstore.NewBufferedTxidList(db, 1000),
		tokenCallback: make(map[string]CallbackInfo),
		minerSubtrees: make([][]*MinerSubtree, cfg.NumMiners),
		stumpStore:    diskstore.NewDiskStumpStore(db),
		txidIndex:     diskstore.NewBufferedTxIDIndex(db, 1000),
		tokenReg:      stump.NewTokenRegistry(),
		blockCh:       make(chan *BlockFinalizedEvent, 1),
		minerSubStore: diskstore.NewMinerSubtreeStore(db),
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

	// Register token and index txid→token.
	r.tokenReg.Register(token)
	r.txidIndex.Set(txid, token)

	r.txidList.Append(txid)
	n := r.txidList.Len()

	r.mu.Lock()
	r.tokenCallback[token] = cb
	cfg := r.cfg

	// Seal a subtree every HashesPerSubtree txids.
	if n%cfg.HashesPerSubtree == 0 {
		subtreeIdx := (n / cfg.HashesPerSubtree) - 1
		start := subtreeIdx * cfg.HashesPerSubtree
		base := r.txidList.Slice(start, n)
		r.mu.Unlock()

		r.sealSubtree(subtreeIdx, base)
	} else {
		r.mu.Unlock()
	}

	// Check for block completion after lock is released.
	n2 := r.txidList.Len()

	if n2 == cfg.HashesPerBlock {
		return r.finalizeBlock()
	}
	return nil
}

// AddTxIDDirect is the in-process fast path for large-scale runs.
// It bypasses JSON/HTTP overhead and accepts pre-computed txid hashes directly.
// Returns a BlockFinalizedEvent when the block is complete.
func (r *Registry) AddTxIDDirect(txid chainhash.Hash, token string, cb CallbackInfo) *BlockFinalizedEvent {
	// Register token and index txid→token.
	r.tokenReg.Register(token)
	r.txidIndex.Set(txid, token)

	r.txidList.Append(txid)
	n := r.txidList.Len()

	r.mu.Lock()
	r.tokenCallback[token] = cb
	cfg := r.cfg

	// Seal a subtree every HashesPerSubtree txids.
	if n%cfg.HashesPerSubtree == 0 {
		subtreeIdx := (n / cfg.HashesPerSubtree) - 1
		start := subtreeIdx * cfg.HashesPerSubtree
		base := r.txidList.Slice(start, n)
		r.mu.Unlock()

		r.sealSubtree(subtreeIdx, base)
	} else {
		r.mu.Unlock()
	}

	// Check for block completion after lock is released.
	n2 := r.txidList.Len()

	if n2 == cfg.HashesPerBlock {
		return r.finalizeBlock()
	}
	return nil
}

// sealSubtree builds all miners' versions of the subtree and pre-computes
// miner-0 proofs using the STUMP XOR index. Called without the registry lock.
//
// ## Indexing strategy
//
// Instead of the old approach (iterate every token's txid list to find which
// belong to this subtree), we use the txidIndex to look up each txid's token
// in O(1), then group proofs by XOR(tokenHash, subtreeRoot) for O(1) storage
// and later O(1) retrieval at block announcement.
//
// This changes the per-subtree work from O(tokens × txids/token) to
// O(hashesPerSubtree) — a dramatic improvement at scale.
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

	// Pre-compute miner-0 proofs using STUMP XOR indexing.
	t1 := time.Now()
	miner0 := minerSubs[0]

	// Build a reverse-index: txid → local position in miner-0's ordering.
	localIdx := make(map[chainhash.Hash]int, cfg.HashesPerSubtree)
	for i, h := range miner0.Leaves {
		localIdx[h] = i
	}

	start := subtreeIdx * cfg.HashesPerSubtree

	// Group proofs by XOR key using the txidIndex (O(1) per txid lookup).
	tokenProofsByKey := make(map[stump.Key][]*stump.Entry)

	// Flush buffered txid→token writes before reading them back.
	if buf, ok := r.txidIndex.(*diskstore.BufferedTxIDIndex); ok {
		buf.Flush()
	}

	for _, txid := range baseTxids {
		token, ok := r.txidIndex.Get(txid)
		if !ok {
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

		th, _ := r.tokenReg.Hash(token)
		key := stump.XORKey(th, miner0.Root)
		tokenProofsByKey[key] = append(tokenProofsByKey[key], &stump.Entry{
			TxID:        txid,
			SubtreeIdx:  subtreeIdx,
			LocalIdx:    li,
			GlobalIdx:   start + li,
			SiblingPath: sp,
		})
	}

	// Batch-append STUMP entries to disk.
	for key, entries := range tokenProofsByKey {
		r.stumpStore.AppendBatch(key, entries)
	}

	r.mc.RecordProofCompute(time.Since(t1))

	slog.Info("proofs pre-computed (STUMP indexed)",
		"subtreeIdx", subtreeIdx,
		"stumpKeys", len(tokenProofsByKey),
		"proofDuration", time.Since(t1),
	)

	// Evict MinerSubtree internals to disk to free RAM.
	// Must happen AFTER proof pre-computation which reads Leaves/Store.
	for m := 0; m < cfg.NumMiners; m++ {
		ms := minerSubs[m]
		r.minerSubStore.Save(m, subtreeIdx, hashSliceToBytes(ms.Leaves), hashSliceToBytes(ms.Store))
		ms.Leaves = nil
		ms.Store = nil
	}
}

// finalizeBlock simulates block discovery: replaces the coinbase placeholder
// in subtree-0, re-seals it, recomputes affected proofs, then uses STUMP
// Discover to gather proofs for all tokens from the XOR index.
func (r *Registry) finalizeBlock() *BlockFinalizedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	slog.Info("block complete", "txids", r.txidList.Len())

	// ── Coinbase replacement ────────────────────────────────────────────────
	t0 := time.Now()

	var coinbase chainhash.Hash
	if _, err := rand.Read(coinbase[:]); err != nil {
		slog.Error("coinbase: random generation failed", "err", err)
	}
	oldCoinbase, _ := r.txidList.Get(0)
	r.txidList.Set(0, coinbase)

	slog.Info("coinbase replaced",
		"old", hex.EncodeToString(oldCoinbase[:8]),
		"new", hex.EncodeToString(coinbase[:8]),
	)

	// Reload subtree-0 leaves/store from disk if evicted.
	for m := 0; m < r.cfg.NumMiners; m++ {
		ms := r.minerSubtrees[m][0]
		if ms.Leaves == nil {
			leavesBytes, storeBytes, ok := r.minerSubStore.Load(m, 0)
			if !ok {
				slog.Error("coinbase: failed to reload subtree-0", "miner", m)
				continue
			}
			ms.Leaves = bytesToHashSlice(leavesBytes)
			ms.Store = bytesToHashSlice(storeBytes)
		}
	}

	// Re-seal subtree-0 for every miner: replace leaf-0, rebuild store+root.
	oldSubtree0Root := r.minerSubtrees[0][0].Root
	for m := 0; m < r.cfg.NumMiners; m++ {
		ms := r.minerSubtrees[m][0]
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

	newSubtree0Root := r.minerSubtrees[0][0].Root

	// Recompute subtree-0 proofs using the stumpStore (no in-memory tokenProofs needed).
	// For each token, probe the old XOR key to get subtree-0 entries,
	// recompute proofs with the new leaves, and insert under the new XOR key.
	miner0 := r.minerSubtrees[0][0]
	localIdx := make(map[chainhash.Hash]int, r.cfg.HashesPerSubtree)
	for i, h := range miner0.Leaves {
		localIdx[h] = i
	}

	recomputedProofs := 0
	tokens := r.tokenReg.Tokens()
	for _, token := range tokens {
		th, _ := r.tokenReg.Hash(token)
		oldKey := stump.XORKey(th, oldSubtree0Root)
		oldEntries := r.stumpStore.Get(oldKey)
		if len(oldEntries) == 0 {
			continue
		}

		newKey := stump.XORKey(th, newSubtree0Root)
		newEntries := make([]*stump.Entry, 0, len(oldEntries))
		for _, e := range oldEntries {
			txid := e.TxID
			if txid == oldCoinbase {
				txid = coinbase
			}
			li, ok := localIdx[txid]
			if !ok {
				slog.Error("coinbase reseal: txid not found in miner-0 subtree-0",
					"token", token, "txid", hex.EncodeToString(txid[:8]))
				continue
			}
			sp, err := subtree.GetProofFromStore(miner0.Leaves, miner0.Store, li)
			if err != nil {
				slog.Error("coinbase reseal: proof recomputation failed",
					"token", token, "err", err)
				continue
			}
			newEntries = append(newEntries, &stump.Entry{
				TxID:        txid,
				SubtreeIdx:  0,
				LocalIdx:    li,
				GlobalIdx:   li,
				SiblingPath: sp,
			})
			recomputedProofs++
		}
		if len(newEntries) > 0 {
			r.stumpStore.AppendBatch(newKey, newEntries)
		}
	}

	coinbaseDur := time.Since(t0)
	r.mc.RecordCoinbaseReseal(coinbaseDur)
	slog.Info("coinbase reseal complete",
		"duration", coinbaseDur,
		"recomputedProofs", recomputedProofs,
	)

	// ── STUMP Discovery ─────────────────────────────────────────────────────
	// Use the XOR probe to gather all proofs per token.
	// This replaces the old approach of copying tokenProofs directly.
	t1 := time.Now()

	m0 := r.minerSubtrees[0]
	roots := make([]chainhash.Hash, len(m0))
	for i, ms := range m0 {
		roots[i] = ms.Root
	}

	// Discover matching STUMPs: O(subtrees × tokens) XOR probes.
	discovered := stump.Discover(r.stumpStore, r.tokenReg, roots)

	slog.Info("STUMP discovery complete",
		"tokens", len(discovered),
		"duration", time.Since(t1),
	)

	// Convert discovered entries to SubtreeProofs for BUMP assembly.
	proofs := make(map[string][]*SubtreeProof, len(discovered))
	for tok, entries := range discovered {
		sp := make([]*SubtreeProof, len(entries))
		for i, e := range entries {
			sp[i] = &SubtreeProof{
				TxID:        e.TxID,
				SubtreeIdx:  e.SubtreeIdx,
				LocalIdx:    e.LocalIdx,
				GlobalIdx:   e.GlobalIdx,
				SiblingPath: e.SiblingPath,
			}
		}
		proofs[tok] = sp
	}

	cbs := make(map[string]CallbackInfo, len(r.tokenCallback))
	for k, v := range r.tokenCallback {
		cbs[k] = v
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

// hashSliceToBytes serialises a slice of chainhash.Hash into a flat byte slice.
func hashSliceToBytes(hashes []chainhash.Hash) []byte {
	buf := make([]byte, len(hashes)*32)
	for i, h := range hashes {
		copy(buf[i*32:], h[:])
	}
	return buf
}

// bytesToHashSlice deserialises a flat byte slice into a slice of chainhash.Hash.
func bytesToHashSlice(data []byte) []chainhash.Hash {
	n := len(data) / 32
	result := make([]chainhash.Hash, n)
	for i := 0; i < n; i++ {
		copy(result[i][:], data[i*32:])
	}
	return result
}
