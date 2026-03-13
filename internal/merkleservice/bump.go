package merkleservice

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	gosdk "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// subtreeCache is a bounded LRU-ish cache for loaded subtree data.
type subtreeCache struct {
	mu      sync.Mutex
	entries map[int]*cachedSubtree
	order   []int // access order for eviction
	maxSize int   // max entries
	mc      *metrics.Collector
}

type cachedSubtree struct {
	Leaves []chainhash.Hash
	Store  []chainhash.Hash
}

func newSubtreeCache(maxEntries int, mc *metrics.Collector) *subtreeCache {
	return &subtreeCache{
		entries: make(map[int]*cachedSubtree, maxEntries),
		order:   make([]int, 0, maxEntries),
		maxSize: maxEntries,
		mc:      mc,
	}
}

func (sc *subtreeCache) get(si int) *cachedSubtree {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if e, ok := sc.entries[si]; ok {
		sc.mc.RecordCacheHit()
		return e
	}
	return nil
}

func (sc *subtreeCache) put(si int, cs *cachedSubtree) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if _, ok := sc.entries[si]; ok {
		return // already present
	}
	// Evict oldest if at capacity.
	if len(sc.entries) >= sc.maxSize && sc.maxSize > 0 {
		oldest := sc.order[0]
		sc.order = sc.order[1:]
		delete(sc.entries, oldest)
	}
	sc.entries[si] = cs
	sc.order = append(sc.order, si)
}

// ProcessBUMPs is the entry-point for block-finalization BUMP work.
// It loads subtrees from disk (with caching), computes proofs JIT, and
// records BUMP sizes and timing. No HTTP delivery.
func ProcessBUMPs(
	blockHeight uint32,
	subtreeHeight, topTreeHeight, totalHeight int,
	mc *metrics.Collector,
	evt *BlockFinalizedEvent,
	dumpFile string,
	maxCacheEntries int,
) {
	t0 := time.Now()

	// Pre-compute top-tree proofs for every subtree index once.
	topProofs, err := subtree.GetAllProofs(evt.SubtreeRoots)
	if err != nil {
		slog.Error("top tree proof failed", "err", err)
		return
	}

	mc.RecordTopTreeBuild(time.Since(t0))
	slog.Info("top tree proofs ready",
		"numSubtrees", len(evt.SubtreeRoots),
		"duration", time.Since(t0),
	)

	// Build list of all business tokens.
	t1 := time.Now()
	tokens := make([]string, evt.NumBusinesses)
	for i := 0; i < evt.NumBusinesses; i++ {
		tokens[i] = fmt.Sprintf("token-%d", i)
	}

	total := len(tokens)
	numWorkers := runtime.GOMAXPROCS(0)
	if numWorkers > total {
		numWorkers = total
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	slog.Info("BUMP assembly starting",
		"tokens", total,
		"workers", numWorkers,
		"cacheSize", maxCacheEntries,
	)

	// Disk-backed subtree loading with bounded cache.
	cache := newSubtreeCache(maxCacheEntries, mc)

	loadSubtree := func(si int) *cachedSubtree {
		// Check cache first.
		if cs := cache.get(si); cs != nil {
			return cs
		}
		mc.RecordCacheMiss()

		// Load from disk.
		tRead := time.Now()
		leavesBytes, storeBytes, ok := evt.MinerSubStore.Load(evt.WinnerMiner, si)
		mc.RecordDiskRead(time.Since(tRead))
		if !ok {
			slog.Error("subtree load failed", "miner", evt.WinnerMiner, "subtree", si)
			return nil
		}
		cs := &cachedSubtree{
			Leaves: bytesToHashSlice(leavesBytes),
			Store:  bytesToHashSlice(storeBytes),
		}
		cache.put(si, cs)
		return cs
	}

	// Worker pool for BUMP assembly.
	workCh := make(chan int, total)
	var wg sync.WaitGroup
	var dumpOnce sync.Once

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range workCh {
				token := tokens[idx]

				// Look up subtree positions for this token.
				subtreeIdxs := evt.TokenSubtreeIdx.SubtreeIndices(token)
				if len(subtreeIdxs) == 0 {
					continue
				}

				var proofs []*SubtreeProof
				for _, si := range subtreeIdxs {
					localIdxs := evt.TokenSubtreeIdx.Get(token, si)
					if len(localIdxs) == 0 {
						continue
					}
					sc := loadSubtree(si)
					if sc == nil {
						continue
					}
					for _, li32 := range localIdxs {
						li := int(li32)
						sp, err := subtree.GetProofFromStore(sc.Leaves, sc.Store, li)
						if err != nil {
							continue
						}
						proofs = append(proofs, &SubtreeProof{
							TxID:        sc.Leaves[li],
							SubtreeIdx:  si,
							LocalIdx:    li,
							GlobalIdx:   si*evt.HashesPerSubtree + li,
							SiblingPath: sp,
						})
					}
				}
				if len(proofs) == 0 {
					continue
				}

				bump, err := buildCompoundBUMP(
					blockHeight, proofs, topProofs,
					subtreeHeight, topTreeHeight, totalHeight,
				)
				if err != nil {
					slog.Error("BUMP build failed", "token", token, "err", err)
					continue
				}
				bumpBytes := bump.Bytes()
				mc.RecordBUMP(len(bumpBytes))

				// Dump first BUMP if requested.
				if dumpFile != "" {
					dumpOnce.Do(func() {
						dumpBUMPHex(dumpFile, token, bumpBytes)
					})
				}

				// Log progress every 10%.
				count := mc.BUMPCount()
				if total >= 10 && count%(int64(total)/10) == 0 {
					slog.Info("BUMP assembly progress",
						"done", count,
						"total", total,
						"elapsed", time.Since(t1).Round(time.Millisecond),
					)
				}
			}
		}()
	}

	// Enqueue work.
	for i := range tokens {
		workCh <- i
	}
	close(workCh)
	wg.Wait()

	mc.RecordBUMPAssembly(time.Since(t1))
	slog.Info("BUMPs assembled",
		"tokens", total,
		"workers", numWorkers,
		"assemblyDuration", time.Since(t1),
	)
}

// buildCompoundBUMP creates a single MerklePath that covers all of a token's
// txids in one O(n·h) pass — no repeated Combine calls.
func buildCompoundBUMP(
	blockHeight uint32,
	proofs []*SubtreeProof,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
) (*gosdk.MerklePath, error) {
	if len(proofs) == 0 {
		return nil, fmt.Errorf("buildCompoundBUMP: no proofs")
	}

	numProofs := len(proofs)
	merged := make([]map[uint64]*gosdk.PathElement, totalHeight)
	merged[0] = make(map[uint64]*gosdk.PathElement, numProofs*2)
	for i := 1; i < totalHeight; i++ {
		merged[i] = make(map[uint64]*gosdk.PathElement, numProofs)
	}

	for _, proof := range proofs {
		if err := accumulateProof(proof, topProofs, subtreeHeight, topTreeHeight, totalHeight, merged); err != nil {
			return nil, err
		}
	}

	// Prune: remove a node at level h if both its children (at h-1) are present.
	for h := totalHeight - 1; h > 0; h-- {
		for offset := range merged[h] {
			childL := offset * 2
			childR := offset*2 + 1
			_, hasL := merged[h-1][childL]
			_, hasR := merged[h-1][childR]
			if hasL && hasR {
				delete(merged[h], offset)
			}
		}
	}

	path := make([][]*gosdk.PathElement, totalHeight)
	for h := 0; h < totalHeight; h++ {
		level := make([]*gosdk.PathElement, 0, len(merged[h]))
		for _, el := range merged[h] {
			level = append(level, el)
		}
		sort.Slice(level, func(i, j int) bool { return level[i].Offset < level[j].Offset })
		path[h] = level
	}

	return gosdk.NewMerklePath(blockHeight, path), nil
}

// accumulateProof inserts all PathElements for one txid proof into merged.
func accumulateProof(
	proof *SubtreeProof,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
	merged []map[uint64]*gosdk.PathElement,
) error {
	if len(proof.SiblingPath) < subtreeHeight {
		return fmt.Errorf("subtree proof too short: got %d want %d", len(proof.SiblingPath), subtreeHeight)
	}
	si := proof.SubtreeIdx
	if si >= len(topProofs) || len(topProofs[si]) < topTreeHeight {
		return fmt.Errorf("top-tree proof missing for subtree %d", si)
	}

	g := uint64(proof.GlobalIdx) //nolint:gosec

	txidHash := proof.TxID
	merged[0][g] = &gosdk.PathElement{Offset: g, Hash: &txidHash, Txid: boolPtr(true)}
	merged[0][g^1] = &gosdk.PathElement{Offset: g ^ 1, Hash: proof.SiblingPath[0]}

	for k := 1; k < subtreeHeight; k++ {
		sibOffset := (g >> k) ^ 1
		merged[k][sibOffset] = &gosdk.PathElement{Offset: sibOffset, Hash: proof.SiblingPath[k]}
	}

	tp := topProofs[si]
	for k := 0; k < topTreeHeight; k++ {
		blockLevel := subtreeHeight + k
		sibOffset := (g >> blockLevel) ^ 1
		merged[blockLevel][sibOffset] = &gosdk.PathElement{Offset: sibOffset, Hash: tp[k]}
	}

	return nil
}

// dumpBUMPHex writes the BUMP binary as a UTF-8 hex string to path.
func dumpBUMPHex(path, token string, data []byte) {
	if err := os.WriteFile(path, []byte(hex.EncodeToString(data)), 0o644); err != nil {
		slog.Error("dump-bump: write failed", "path", path, "err", err)
		return
	}
	slog.Info("dump-bump: wrote BUMP hex",
		"path", path,
		"token", token,
		"bytes", len(data),
		"hexLen", len(data)*2,
	)
}

// boolPtr returns a pointer to a bool literal.
func boolPtr(b bool) *bool { return &b }
