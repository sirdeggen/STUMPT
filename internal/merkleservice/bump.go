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

// leafPos is a lightweight position record for BUMP assembly.
// No heap allocations — stored in a flat slice.
type leafPos struct {
	subtreeIdx int
	localIdx   int
	globalIdx  int
}

// ProcessBUMPs is the entry-point for block-finalization BUMP work.
// It loads subtrees from disk (with caching), computes proofs JIT directly
// from cached stores (no intermediate proof extraction), and records BUMP
// sizes and timing. No HTTP delivery.
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
		if cs := cache.get(si); cs != nil {
			return cs
		}
		mc.RecordCacheMiss()
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

			// Per-worker reusable buffers.
			var positions []leafPos
			subtrees := make(map[int]*cachedSubtree)

			for idx := range workCh {
				token := tokens[idx]

				subtreeIdxs := evt.TokenSubtreeIdx.SubtreeIndices(token)
				if len(subtreeIdxs) == 0 {
					continue
				}

				// Collect positions and load subtrees (no proof extraction).
				positions = positions[:0]
				for k := range subtrees {
					delete(subtrees, k)
				}

				for _, si := range subtreeIdxs {
					localIdxs := evt.TokenSubtreeIdx.Get(token, si)
					if len(localIdxs) == 0 {
						continue
					}
					sc := loadSubtree(si)
					if sc == nil {
						continue
					}
					subtrees[si] = sc
					for _, li32 := range localIdxs {
						positions = append(positions, leafPos{
							subtreeIdx: si,
							localIdx:   int(li32),
							globalIdx:  si*evt.HashesPerSubtree + int(li32),
						})
					}
				}
				if len(positions) == 0 {
					continue
				}

				bump, err := assembleTokenBUMP(
					blockHeight, positions, subtrees, topProofs,
					subtreeHeight, topTreeHeight, totalHeight,
				)
				if err != nil {
					slog.Error("BUMP build failed", "token", token, "err", err)
					continue
				}
				bumpBytes := bump.Bytes()
				mc.RecordBUMP(len(bumpBytes))

				if dumpFile != "" {
					dumpOnce.Do(func() {
						dumpBUMPHex(dumpFile, token, bumpBytes)
					})
				}

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

// assembleTokenBUMP builds a compound BUMP directly from leaf positions and
// cached subtree data. Key optimizations over the naive approach:
//
//  1. Pre-allocated PathElement pool — one slice replaces ~900k individual allocs
//  2. Direct Hash pointers into cached Leaves/Store — zero hash copies
//  3. Sorted positions + bitset dedup — avoids map[uint64] at level 0 entirely
//  4. Existence checks before alloc — fewer map writes at higher levels
//  5. Shared *bool for Txid markers — 1 alloc instead of n
func assembleTokenBUMP(
	blockHeight uint32,
	positions []leafPos,
	subtrees map[int]*cachedSubtree,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
) (*gosdk.MerklePath, error) {
	n := len(positions)
	if n == 0 {
		return nil, fmt.Errorf("assembleTokenBUMP: no positions")
	}

	// ── Sort positions by globalIdx ─────────────────────────────────────
	// This enables O(n) dedup at level 0: adjacent positions that share
	// a sibling pair (same globalIdx>>1) only emit the sibling once.
	sort.Slice(positions, func(i, j int) bool {
		return positions[i].globalIdx < positions[j].globalIdx
	})

	// ── Pre-allocate element pool ────────────────────────────────────────
	pool := make([]gosdk.PathElement, 0, n*(totalHeight+2))
	alloc := func() *gosdk.PathElement {
		pool = append(pool, gosdk.PathElement{})
		return &pool[len(pool)-1]
	}

	trueVal := true

	// ── Level 0: build sorted directly (no map needed) ──────────────────
	// Because positions are sorted by globalIdx, we can build level 0 as a
	// sorted slice with O(n) dedup instead of O(n) map insertions + O(n log n) sort.
	level0 := make([]*gosdk.PathElement, 0, n*2)
	lastPair := int64(-1) // track which pair (globalIdx>>1) was last emitted

	for _, pos := range positions {
		sc := subtrees[pos.subtreeIdx]
		li := pos.localIdx
		g := uint64(pos.globalIdx) //nolint:gosec
		nLeaves := len(sc.Leaves)
		pair := int64(pos.globalIdx >> 1) //nolint:gosec

		// Txid entry — always emitted (sorted order, no duplicates by construction).
		el := alloc()
		el.Offset = g
		el.Hash = &sc.Leaves[li]
		el.Txid = &trueVal
		level0 = append(level0, el)

		// Sibling — emit only once per pair.
		if pair != lastPair {
			sibLi := li ^ 1
			if sibLi >= nLeaves {
				sibLi = li
			}
			sibEl := alloc()
			sibEl.Offset = g ^ 1
			sibEl.Hash = &sc.Leaves[sibLi]
			level0 = append(level0, sibEl)
			lastPair = pair
		}
	}

	// Sort level 0 by offset (txids and siblings are interleaved).
	sort.Slice(level0, func(i, j int) bool { return level0[i].Offset < level0[j].Offset })

	// Build a fast offset→presence lookup for level 0 (used by pruning).
	level0Set := make(map[uint64]struct{}, len(level0))
	for _, el := range level0 {
		level0Set[el.Offset] = struct{}{}
	}

	// ── Levels 1+ : maps for dedup (much smaller than level 0) ──────────
	merged := make([]map[uint64]*gosdk.PathElement, totalHeight)
	merged[0] = nil // level 0 handled above
	for i := 1; i < totalHeight; i++ {
		merged[i] = make(map[uint64]*gosdk.PathElement, n)
	}

	for _, pos := range positions {
		sc := subtrees[pos.subtreeIdx]
		g := uint64(pos.globalIdx) //nolint:gosec
		li := pos.localIdx
		nLeaves := len(sc.Leaves)

		// Subtree levels 1..subtreeHeight-1.
		pad := subtree.NextPowerOfTwo(nLeaves)
		storeOff := 0
		storeSize := pad / 2
		for k := 1; k < subtreeHeight; k++ {
			sibOff := (g >> k) ^ 1
			if _, exists := merged[k][sibOff]; !exists {
				sibPos := (li >> k) ^ 1
				if sibPos >= storeSize {
					sibPos = storeSize - 1
				}
				el := alloc()
				el.Offset = sibOff
				el.Hash = &sc.Store[storeOff+sibPos]
				merged[k][sibOff] = el
			}
			storeOff += storeSize
			storeSize /= 2
		}

		// Top tree levels.
		tp := topProofs[pos.subtreeIdx]
		for k := 0; k < topTreeHeight; k++ {
			blockLevel := subtreeHeight + k
			sibOff := (g >> blockLevel) ^ 1
			if _, exists := merged[blockLevel][sibOff]; !exists {
				el := alloc()
				el.Offset = sibOff
				el.Hash = tp[k]
				merged[blockLevel][sibOff] = el
			}
		}
	}

	// ── Prune: remove node at level h if both children at h-1 present ───
	for h := totalHeight - 1; h > 0; h-- {
		var hasChild func(level int, offset uint64) bool
		if h == 1 {
			// Level 1 checks children against level 0 (set-based).
			hasChild = func(_ int, offset uint64) bool {
				_, ok := level0Set[offset]
				return ok
			}
		} else {
			hasChild = func(level int, offset uint64) bool {
				_, ok := merged[level][offset]
				return ok
			}
		}
		for offset := range merged[h] {
			childL := offset * 2
			childR := offset*2 + 1
			if hasChild(h-1, childL) && hasChild(h-1, childR) {
				delete(merged[h], offset)
			}
		}
	}

	// ── Build sorted path ───────────────────────────────────────────────
	path := make([][]*gosdk.PathElement, totalHeight)
	path[0] = level0 // already sorted
	for h := 1; h < totalHeight; h++ {
		level := make([]*gosdk.PathElement, 0, len(merged[h]))
		for _, el := range merged[h] {
			level = append(level, el)
		}
		sort.Slice(level, func(i, j int) bool { return level[i].Offset < level[j].Offset })
		path[h] = level
	}

	return gosdk.NewMerklePath(blockHeight, path), nil
}

// ── Legacy functions (kept for benchmark tests) ──────────────────────────────

// buildCompoundBUMP creates a single MerklePath that covers all of a token's
// txids in one O(n·h) pass — no repeated Combine calls.
// Used by benchmark tests; production code uses assembleTokenBUMP.
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

func boolPtr(b bool) *bool { return &b }
