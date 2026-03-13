package merkleservice

import (
	"crypto/rand"
	"encoding/binary"
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
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// SealSubtreesToDisk is Phase 2 of the pipeline: for each subtree boundary,
// jitter txids per miner, build merkle stores, save to disk, and index
// token→leaf positions in each miner's TokenSubtreeIndex.
//
// Returns per-miner subtree roots (roots[minerIdx][subtreeIdx]).
// The txids slice may be nilled by the caller after this returns.
func SealSubtreesToDisk(
	txids []chainhash.Hash,
	cfg *config.Config,
	db *diskstore.DB,
	mc *metrics.Collector,
	minerTokenIdx []*TokenSubtreeIndex,
) [][]chainhash.Hash {
	numSubtrees := cfg.NumSubtrees()
	minerSubStore := diskstore.NewMinerSubtreeStore(db)

	// Per-miner roots.
	roots := make([][]chainhash.Hash, cfg.NumMiners)
	for m := 0; m < cfg.NumMiners; m++ {
		roots[m] = make([]chainhash.Hash, numSubtrees)
	}

	for si := 0; si < numSubtrees; si++ {
		start := si * cfg.HashesPerSubtree
		end := start + cfg.HashesPerSubtree
		base := txids[start:end]

		t0 := time.Now()

		// Build all miners' subtrees in parallel.
		minerSubs := make([]*MinerSubtree, cfg.NumMiners)
		var wg sync.WaitGroup
		wg.Add(cfg.NumMiners)
		for m := 0; m < cfg.NumMiners; m++ {
			go func(m int) {
				defer wg.Done()
				leaves := jitterTxids(base, m, si, cfg.JitterPercent)
				store := subtree.BuildMerkleStore(leaves)
				root := store[len(store)-1]
				minerSubs[m] = &MinerSubtree{
					Index:  si,
					Leaves: leaves,
					Root:   root,
					Store:  store,
				}
			}(m)
		}
		wg.Wait()

		sealDur := time.Since(t0)
		mc.RecordSubtreeSeal(sealDur)

		// Save all miners' subtrees to disk.
		tDisk := time.Now()
		for m := 0; m < cfg.NumMiners; m++ {
			ms := minerSubs[m]
			roots[m][si] = ms.Root

			// Serialize leaves and store as flat byte slices.
			leavesBytes := hashSliceToBytes(ms.Leaves)
			storeBytes := hashSliceToBytes(ms.Store)
			if err := minerSubStore.Save(m, si, leavesBytes, storeBytes); err != nil {
				slog.Error("disk write failed", "miner", m, "subtree", si, "err", err)
			}
		}
		mc.RecordDiskWrite(time.Since(tDisk))

		// Index token→leaf positions for every miner.
		tIdx := time.Now()
		for m := 0; m < cfg.NumMiners; m++ {
			ms := minerSubs[m]

			// Build reverse index: txid → local position in this miner's ordering.
			localIdx := make(map[chainhash.Hash]int, len(ms.Leaves))
			for i, h := range ms.Leaves {
				localIdx[h] = i
			}

			// For each leaf in canonical order, derive token from global index.
			tokenLocalIdxs := make(map[string][]int32)
			for i, txid := range base {
				globalIdx := start + i
				token := fmt.Sprintf("token-%d", globalIdx%cfg.NumBusinesses)
				li, ok := localIdx[txid]
				if !ok {
					continue
				}
				tokenLocalIdxs[token] = append(tokenLocalIdxs[token], int32(li))
			}
			for tok, idxs := range tokenLocalIdxs {
				minerTokenIdx[m].AddBatch(tok, si, idxs)
			}
		}
		mc.RecordProofCompute(time.Since(tIdx))

		// Free subtree data — it's on disk now.
		for m := range minerSubs {
			minerSubs[m] = nil
		}

		if si%10 == 0 || si == numSubtrees-1 {
			slog.Info("subtree sealed",
				"subtreeIdx", si,
				"of", numSubtrees,
				"sealMs", sealDur.Milliseconds(),
			)
		}
	}

	return roots
}

// FinalizeBlock simulates block discovery: randomly selects a winning miner,
// loads the winner's subtree-0 from disk, replaces the coinbase, reseals it,
// saves back, and returns the finalization event.
func FinalizeBlock(
	cfg *config.Config,
	mc *metrics.Collector,
	db *diskstore.DB,
	minerRoots [][]chainhash.Hash,
	minerTokenIdx []*TokenSubtreeIndex,
	firstTxid chainhash.Hash,
) *BlockFinalizedEvent {
	t0 := time.Now()
	minerSubStore := diskstore.NewMinerSubtreeStore(db)

	// ── Select winning miner (random, simulating real mining) ──────────────
	winner := 0
	if cfg.NumMiners > 1 {
		var rb [8]byte
		if _, err := rand.Read(rb[:]); err == nil {
			winner = int(binary.LittleEndian.Uint64(rb[:]) % uint64(cfg.NumMiners))
		}
	}

	slog.Info("block winner selected", "miner", winner, "totalMiners", cfg.NumMiners)

	// ── Coinbase replacement ────────────────────────────────────────────────
	var coinbase chainhash.Hash
	if _, err := rand.Read(coinbase[:]); err != nil {
		slog.Error("coinbase: random generation failed", "err", err)
	}

	slog.Info("coinbase replacing",
		"old", hex.EncodeToString(firstTxid[:8]),
		"new", hex.EncodeToString(coinbase[:8]),
	)

	// Load winner's subtree-0 from disk.
	leavesBytes, _, ok := minerSubStore.Load(winner, 0)
	if !ok {
		slog.Error("coinbase: failed to load winner subtree-0 from disk")
		return nil
	}
	leaves := bytesToHashSlice(leavesBytes)

	// Replace old coinbase with new one.
	replaced := false
	for i, h := range leaves {
		if h == firstTxid {
			leaves[i] = coinbase
			replaced = true
			break
		}
	}
	if !replaced {
		slog.Error("coinbase: old coinbase not found in winning miner subtree-0", "miner", winner)
	}

	// Rebuild store and root.
	newStore := subtree.BuildMerkleStore(leaves)
	newRoot := newStore[len(newStore)-1]
	minerRoots[winner][0] = newRoot

	// Save resealed subtree-0 back to disk.
	if err := minerSubStore.Save(winner, 0, hashSliceToBytes(leaves), hashSliceToBytes(newStore)); err != nil {
		slog.Error("coinbase: failed to save resealed subtree-0", "err", err)
	}

	coinbaseDur := time.Since(t0)
	mc.RecordCoinbaseReseal(coinbaseDur)
	slog.Info("coinbase reseal complete", "miner", winner, "duration", coinbaseDur)

	return &BlockFinalizedEvent{
		WinnerMiner:      winner,
		SubtreeRoots:     minerRoots[winner],
		TokenSubtreeIdx:  minerTokenIdx[winner],
		MinerSubStore:    minerSubStore,
		HashesPerSubtree: cfg.HashesPerSubtree,
		NumBusinesses:    cfg.NumBusinesses,
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// hashSliceToBytes converts []chainhash.Hash to a flat byte slice.
func hashSliceToBytes(hashes []chainhash.Hash) []byte {
	buf := make([]byte, len(hashes)*chainhash.HashSize)
	for i, h := range hashes {
		copy(buf[i*chainhash.HashSize:], h[:])
	}
	return buf
}

// bytesToHashSlice converts a flat byte slice back to []chainhash.Hash.
func bytesToHashSlice(data []byte) []chainhash.Hash {
	n := len(data) / chainhash.HashSize
	result := make([]chainhash.Hash, n)
	for i := 0; i < n; i++ {
		copy(result[i][:], data[i*chainhash.HashSize:(i+1)*chainhash.HashSize])
	}
	return result
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
