package merkleservice

import (
	"bytes"
	"encoding/binary"
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

// ── Fast BUMP types ────────────────────────────────────────────────────────

// bumpEntry is a compact entry for BUMP output. Points into cached data — no copies.
type bumpEntry struct {
	offset uint64
	hash   *chainhash.Hash // pointer into cached leaves/store (zero-copy)
	txid   bool
}

// workerState holds reusable per-worker buffers to eliminate GC pressure.
type workerState struct {
	levels [][]bumpEntry  // per-level sorted entry lists
	bitset []uint64       // reusable bitset (sized for largest subtree level)
	outBuf bytes.Buffer   // BUMP binary output buffer
	posBuf []int          // scratch for grouping positions by subtree
}

func newWorkerState(totalHeight int) *workerState {
	ws := &workerState{
		levels: make([][]bumpEntry, totalHeight),
	}
	for i := range ws.levels {
		ws.levels[i] = make([]bumpEntry, 0, 256)
	}
	return ws
}

// reset clears all level slices for reuse, keeping backing arrays.
func (ws *workerState) reset(totalHeight int) {
	for i := 0; i < totalHeight; i++ {
		if i < len(ws.levels) {
			ws.levels[i] = ws.levels[i][:0]
		} else {
			ws.levels = append(ws.levels, make([]bumpEntry, 0, 256))
		}
	}
	ws.outBuf.Reset()
}

// ensureBitset returns a bitset of at least nWords uint64s, cleared.
func (ws *workerState) ensureBitset(nWords int) []uint64 {
	if cap(ws.bitset) < nWords {
		ws.bitset = make([]uint64, nWords)
	} else {
		ws.bitset = ws.bitset[:nWords]
		for i := range ws.bitset {
			ws.bitset[i] = 0
		}
	}
	return ws.bitset
}

// bitsetCheck returns true if bit at pos is set, and sets it.
func bitsetCheck(bs []uint64, pos int) bool {
	word := pos / 64
	bit := uint64(1) << (pos % 64)
	if bs[word]&bit != 0 {
		return true
	}
	bs[word] |= bit
	return false
}

// extractSubtreeFragment computes the subtree-level BUMP entries (levels 0..subtreeHeight-1)
// for a single token's txids in a single subtree. Used during Phase 2 sealing when
// leaves and store are already in memory. Returns per-level bumpEntry slices with
// hash bytes copied (not pointers, since the source data will be freed).
func extractSubtreeFragment(
	leaves []chainhash.Hash,
	store []chainhash.Hash,
	localIdxs []int32,
	subtreeIdx int,
	hashesPerSubtree int,
	subtreeHeight int,
	ws *workerState,
) [][]bumpEntry {
	nLeaves := len(leaves)
	pad := subtree.NextPowerOfTwo(nLeaves)

	// Sort local indices for consistent output order.
	sortedIdxs := make([]int, len(localIdxs))
	for i, li := range localIdxs {
		sortedIdxs[i] = int(li)
	}
	sort.Ints(sortedIdxs)

	// Ensure we have enough levels in ws.
	for len(ws.levels) < subtreeHeight {
		ws.levels = append(ws.levels, nil)
	}
	for k := 0; k < subtreeHeight; k++ {
		ws.levels[k] = ws.levels[k][:0]
	}

	// ── Level 0: txid + sibling entries ─────────────────────────────────
	bsWords0 := (pad/2 + 63) / 64
	bs0 := ws.ensureBitset(bsWords0)

	for _, li := range sortedIdxs {
		g := uint64(subtreeIdx*hashesPerSubtree + li) //nolint:gosec

		// Txid entry — always emitted.
		ws.levels[0] = append(ws.levels[0], bumpEntry{
			offset: g,
			hash:   &leaves[li],
			txid:   true,
		})

		// Sibling — emit only once per pair.
		pairLocal := li >> 1
		if !bitsetCheck(bs0, pairLocal) {
			sibLi := li ^ 1
			if sibLi >= nLeaves {
				sibLi = li
			}
			ws.levels[0] = append(ws.levels[0], bumpEntry{
				offset: g ^ 1,
				hash:   &leaves[sibLi],
			})
		}
	}

	// ── Subtree levels 1..subtreeHeight-1 ───────────────────────────────
	storeOff := 0
	storeSize := pad / 2
	for k := 1; k < subtreeHeight; k++ {
		bsWordsK := (storeSize + 63) / 64
		bsK := ws.ensureBitset(bsWordsK)

		for _, li := range sortedIdxs {
			g := uint64(subtreeIdx*hashesPerSubtree + li) //nolint:gosec

			sibLocalK := (li >> k) ^ 1
			if sibLocalK >= storeSize {
				sibLocalK = storeSize - 1
			}

			if !bitsetCheck(bsK, sibLocalK) {
				sibGlobalK := (g >> k) ^ 1
				ws.levels[k] = append(ws.levels[k], bumpEntry{
					offset: sibGlobalK,
					hash:   &store[storeOff+sibLocalK],
				})
			}
		}

		storeOff += storeSize
		storeSize /= 2
	}

	// Sort each level by offset.
	for k := 0; k < subtreeHeight; k++ {
		lev := ws.levels[k]
		if len(lev) > 1 {
			sort.Slice(lev, func(i, j int) bool { return lev[i].offset < lev[j].offset })
		}
	}

	// Copy into owned slices (source data will be freed after Phase 2).
	result := make([][]bumpEntry, subtreeHeight)
	for k := 0; k < subtreeHeight; k++ {
		result[k] = make([]bumpEntry, len(ws.levels[k]))
		copy(result[k], ws.levels[k])
	}
	return result
}

// MarshalFragment encodes per-level bumpEntry slices into a flat byte buffer.
// Format: [1B numLevels] per level: [4B LE numEntries] per entry: [8B LE offset][1B flags][32B hash]
func MarshalFragment(levels [][]bumpEntry) []byte {
	// Calculate total size.
	size := 1 // numLevels
	for _, lev := range levels {
		size += 4 + len(lev)*41 // 4B count + entries
	}

	buf := make([]byte, size)
	off := 0
	buf[off] = byte(len(levels))
	off++

	for _, lev := range levels {
		binary.LittleEndian.PutUint32(buf[off:], uint32(len(lev)))
		off += 4
		for _, e := range lev {
			binary.LittleEndian.PutUint64(buf[off:], e.offset)
			off += 8
			if e.txid {
				buf[off] = 2 // bit 1 = txid
			} else {
				buf[off] = 0
			}
			off++
			copy(buf[off:], e.hash[:])
			off += 32
		}
	}
	return buf
}

// UnmarshalFragment decodes a fragment into per-level bumpEntry slices.
// The hash fields point into newly allocated chainhash.Hash values.
func UnmarshalFragment(data []byte) ([][]bumpEntry, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("fragment too short")
	}

	numLevels := int(data[0])
	off := 1
	levels := make([][]bumpEntry, numLevels)

	for k := 0; k < numLevels; k++ {
		if off+4 > len(data) {
			return nil, fmt.Errorf("fragment truncated at level %d header", k)
		}
		n := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4

		need := n * 41
		if off+need > len(data) {
			return nil, fmt.Errorf("fragment truncated at level %d entries", k)
		}

		entries := make([]bumpEntry, n)
		// Allocate all hashes in one block for cache locality.
		hashes := make([]chainhash.Hash, n)
		for i := 0; i < n; i++ {
			entries[i].offset = binary.LittleEndian.Uint64(data[off:])
			off += 8
			entries[i].txid = data[off]&2 != 0
			off++
			copy(hashes[i][:], data[off:off+32])
			entries[i].hash = &hashes[i]
			off += 32
		}
		levels[k] = entries
	}
	return levels, nil
}

// assembleTokenBUMPFromFragments builds a BUMP from pre-computed STUMP fragments.
// No full subtree loads or merkle store walks — just fragment deserialization,
// top-tree addition, pruning, and serialization.
func assembleTokenBUMPFromFragments(
	blockHeight uint32,
	fragmentData [][]byte, // one per subtree the token spans
	subtreeIdxs []int,     // corresponding subtree indices
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
	hashesPerSubtree int,
	ws *workerState,
) ([]byte, error) {
	if len(fragmentData) == 0 {
		return nil, fmt.Errorf("assembleTokenBUMPFromFragments: no fragments")
	}

	ws.reset(totalHeight)

	// ── Load and concatenate subtree-level entries from fragments ────────
	for _, frag := range fragmentData {
		levels, err := UnmarshalFragment(frag)
		if err != nil {
			return nil, fmt.Errorf("fragment unmarshal: %w", err)
		}
		for k := 0; k < len(levels) && k < subtreeHeight; k++ {
			ws.levels[k] = append(ws.levels[k], levels[k]...)
		}
	}

	// ── Top-tree levels: tiny map for dedup ─────────────────────────────
	// We need globalIdx values to compute top-tree offsets. Extract them from
	// the level-0 txid entries (which have the correct global offsets).
	for k := 0; k < topTreeHeight; k++ {
		blockLevel := subtreeHeight + k
		seen := make(map[uint64]struct{}, 256)

		for _, e := range ws.levels[0] {
			if !e.txid {
				continue
			}
			g := e.offset // level-0 txid offset IS the global leaf index
			sibOff := (g >> blockLevel) ^ 1
			if _, exists := seen[sibOff]; !exists {
				seen[sibOff] = struct{}{}
				// Determine which subtree this txid belongs to.
				si := int(g) / hashesPerSubtree
				tp := topProofs[si]
				ws.levels[blockLevel] = append(ws.levels[blockLevel], bumpEntry{
					offset: sibOff,
					hash:   tp[k],
				})
			}
		}
	}

	// ── Sort each level by offset ───────────────────────────────────────
	for h := 0; h < totalHeight; h++ {
		lev := ws.levels[h]
		if len(lev) > 1 {
			sort.Slice(lev, func(i, j int) bool { return lev[i].offset < lev[j].offset })
		}
	}

	// ── Prune: skip level-h entry if both children present at h-1 ───────
	offsetSets := make([]map[uint64]struct{}, totalHeight)
	for h := 0; h < totalHeight; h++ {
		m := make(map[uint64]struct{}, len(ws.levels[h]))
		for _, e := range ws.levels[h] {
			m[e.offset] = struct{}{}
		}
		offsetSets[h] = m
	}

	for h := totalHeight - 1; h > 0; h-- {
		childSet := offsetSets[h-1]
		pruned := ws.levels[h][:0]
		for _, e := range ws.levels[h] {
			childL := e.offset * 2
			childR := e.offset*2 + 1
			_, hasL := childSet[childL]
			_, hasR := childSet[childR]
			if hasL && hasR {
				delete(offsetSets[h], e.offset)
				continue
			}
			pruned = append(pruned, e)
		}
		ws.levels[h] = pruned
	}

	// ── Serialize BUMP binary ───────────────────────────────────────────
	return serializeBUMP(blockHeight, ws.levels[:totalHeight], &ws.outBuf), nil
}

// assembleTokenBUMPFast builds a BUMP using bitset dedup within subtrees
// and flat sorted slice concatenation across subtrees. Returns serialized
// BUMP binary directly — no go-sdk MerklePath intermediary.
func assembleTokenBUMPFast(
	blockHeight uint32,
	positions []leafPos,
	subtrees map[int]*cachedSubtree,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
	ws *workerState,
) ([]byte, error) {
	n := len(positions)
	if n == 0 {
		return nil, fmt.Errorf("assembleTokenBUMPFast: no positions")
	}

	ws.reset(totalHeight)

	// Sort positions by globalIdx.
	sort.Slice(positions, func(i, j int) bool {
		return positions[i].globalIdx < positions[j].globalIdx
	})

	// ── Group positions by subtreeIdx (stable within each group) ────────
	// Since positions are sorted by globalIdx and globalIdx = si*hps + li,
	// positions within the same subtree are already contiguous and sorted.

	// Process subtrees in order. Find contiguous runs.
	start := 0
	for start < n {
		si := positions[start].subtreeIdx
		end := start + 1
		for end < n && positions[end].subtreeIdx == si {
			end++
		}

		sc := subtrees[si]
		if sc == nil {
			start = end
			continue
		}

		nLeaves := len(sc.Leaves)
		pad := subtree.NextPowerOfTwo(nLeaves)

		// ── Level 0: txid + sibling entries ─────────────────────────────
		// Match legacy behavior exactly: always emit txid entries, dedup
		// siblings by pair (globalIdx >> 1). Two entries at the same offset
		// can coexist (one txid, one sibling from a different position).
		bsWords0 := (pad/2 + 63) / 64 // pair-based bitset
		bs0 := ws.ensureBitset(bsWords0)

		for p := start; p < end; p++ {
			li := positions[p].localIdx
			g := uint64(positions[p].globalIdx) //nolint:gosec

			// Txid entry — always emitted (unique globalIdx by construction).
			ws.levels[0] = append(ws.levels[0], bumpEntry{
				offset: g,
				hash:   &sc.Leaves[li],
				txid:   true,
			})

			// Sibling — emit only once per pair (pair = localIdx >> 1).
			pairLocal := li >> 1
			if !bitsetCheck(bs0, pairLocal) {
				sibLi := li ^ 1
				if sibLi >= nLeaves {
					sibLi = li // padding duplicate
				}
				ws.levels[0] = append(ws.levels[0], bumpEntry{
					offset: g ^ 1,
					hash:   &sc.Leaves[sibLi],
				})
			}
		}

		// ── Subtree levels 1..subtreeHeight-1 ───────────────────────────
		storeOff := 0
		storeSize := pad / 2
		for k := 1; k < subtreeHeight; k++ {
			bsWordsK := (storeSize + 63) / 64
			if bsWordsK > bsWords0 {
				bsWordsK = bsWords0 // can't exceed level 0
			}
			bsK := ws.ensureBitset(bsWordsK)

			for p := start; p < end; p++ {
				li := positions[p].localIdx
				g := uint64(positions[p].globalIdx) //nolint:gosec

				sibLocalK := (li >> k) ^ 1
				if sibLocalK >= storeSize {
					sibLocalK = storeSize - 1
				}

				if !bitsetCheck(bsK, sibLocalK) {
					sibGlobalK := (g >> k) ^ 1
					ws.levels[k] = append(ws.levels[k], bumpEntry{
						offset: sibGlobalK,
						hash:   &sc.Store[storeOff+sibLocalK],
					})
				}
			}

			storeOff += storeSize
			storeSize /= 2
		}

		start = end
	}

	// ── Top-tree levels: tiny map for dedup (~208 entries max) ───────────
	for k := 0; k < topTreeHeight; k++ {
		blockLevel := subtreeHeight + k
		seen := make(map[uint64]struct{}, 256)

		for _, pos := range positions {
			g := uint64(pos.globalIdx) //nolint:gosec
			sibOff := (g >> blockLevel) ^ 1
			if _, exists := seen[sibOff]; !exists {
				seen[sibOff] = struct{}{}
				tp := topProofs[pos.subtreeIdx]
				ws.levels[blockLevel] = append(ws.levels[blockLevel], bumpEntry{
					offset: sibOff,
					hash:   tp[k],
				})
			}
		}
	}

	// ── Sort each level by offset ───────────────────────────────────────
	// Subtree levels: entries from different subtrees have non-overlapping
	// offset ranges within the same subtree, but when multiple subtrees
	// contribute to a level, we need a final sort.
	for h := 0; h < totalHeight; h++ {
		lev := ws.levels[h]
		if len(lev) > 1 {
			sort.Slice(lev, func(i, j int) bool { return lev[i].offset < lev[j].offset })
		}
	}

	// ── Prune: skip level-h entry if both children present at h-1 ───────
	// Build offset sets for pruning lookups.
	offsetSets := make([]map[uint64]struct{}, totalHeight)
	for h := 0; h < totalHeight; h++ {
		m := make(map[uint64]struct{}, len(ws.levels[h]))
		for _, e := range ws.levels[h] {
			m[e.offset] = struct{}{}
		}
		offsetSets[h] = m
	}

	for h := totalHeight - 1; h > 0; h-- {
		childSet := offsetSets[h-1]
		pruned := ws.levels[h][:0] // reuse backing array
		for _, e := range ws.levels[h] {
			childL := e.offset * 2
			childR := e.offset*2 + 1
			_, hasL := childSet[childL]
			_, hasR := childSet[childR]
			if hasL && hasR {
				delete(offsetSets[h], e.offset) // also remove from set
				continue
			}
			pruned = append(pruned, e)
		}
		ws.levels[h] = pruned
	}

	// ── Serialize BUMP binary ───────────────────────────────────────────
	return serializeBUMP(blockHeight, ws.levels[:totalHeight], &ws.outBuf), nil
}

// serializeBUMP writes BRC-74 BUMP binary format directly from sorted entry slices.
func serializeBUMP(blockHeight uint32, levels [][]bumpEntry, buf *bytes.Buffer) []byte {
	buf.Reset()
	// Estimate capacity: blockHeight(5) + treeHeight(1) + per-level overhead.
	buf.Grow(64 + len(levels)*8)

	// BlockHeight as VarInt.
	writeVarInt(buf, uint64(blockHeight))
	// Tree height as single byte.
	buf.WriteByte(byte(len(levels)))

	for _, level := range levels {
		// nLeaves as VarInt.
		writeVarInt(buf, uint64(len(level)))
		for _, e := range level {
			// Offset as VarInt.
			writeVarInt(buf, e.offset)
			// Flags: bit 0 = duplicate, bit 1 = txid.
			flags := byte(0)
			if e.txid {
				flags |= 2
			}
			buf.WriteByte(flags)
			// Hash: 32 bytes (no duplicate support in our output — we deduped already).
			buf.Write(e.hash[:])
		}
	}

	return buf.Bytes()
}

// writeVarInt writes a Bitcoin VarInt (BRC-74 compatible) to buf.
func writeVarInt(buf *bytes.Buffer, v uint64) {
	if v < 0xfd {
		buf.WriteByte(byte(v))
		return
	}
	if v < 0x10000 {
		buf.WriteByte(0xfd)
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(v)) //nolint:gosec
		buf.Write(b[:])
		return
	}
	if v < 0x100000000 {
		buf.WriteByte(0xfe)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v)) //nolint:gosec
		buf.Write(b[:])
		return
	}
	buf.WriteByte(0xff)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	buf.Write(b[:])
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

	useFragments := evt.FragStore != nil
	slog.Info("BUMP assembly starting",
		"tokens", total,
		"workers", numWorkers,
		"cacheSize", maxCacheEntries,
		"fragments", useFragments,
	)

	// Worker pool for BUMP assembly.
	workCh := make(chan int, total)
	var wg sync.WaitGroup
	var dumpOnce sync.Once

	if useFragments {
		// ── Fragment-based path: load pre-computed STUMP fragments ───────
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				ws := newWorkerState(totalHeight)
				var fragmentData [][]byte
				var subtreeIdxList []int

				for idx := range workCh {
					token := tokens[idx]

					subtreeIdxs := evt.TokenSubtreeIdx.SubtreeIndices(token)
					if len(subtreeIdxs) == 0 {
						continue
					}

					// Load pre-computed fragments for each subtree.
					fragmentData = fragmentData[:0]
					subtreeIdxList = subtreeIdxList[:0]
					for _, si := range subtreeIdxs {
						tRead := time.Now()
						data, ok := evt.FragStore.Load(evt.WinnerMiner, idx, si)
						mc.RecordDiskRead(time.Since(tRead))
						if !ok {
							mc.RecordCacheMiss()
							continue
						}
						mc.RecordCacheHit()
						fragmentData = append(fragmentData, data)
						subtreeIdxList = append(subtreeIdxList, si)
					}
					if len(fragmentData) == 0 {
						continue
					}

					bumpBytes, err := assembleTokenBUMPFromFragments(
						blockHeight, fragmentData, subtreeIdxList, topProofs,
						subtreeHeight, topTreeHeight, totalHeight,
						evt.HashesPerSubtree, ws,
					)
					if err != nil {
						slog.Error("BUMP build failed", "token", token, "err", err)
						continue
					}
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
	} else {
		// ── Fallback: JIT path (load full subtrees, walk merkle store) ──
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

		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				ws := newWorkerState(totalHeight)
				var positions []leafPos
				subtreeMap := make(map[int]*cachedSubtree)

				for idx := range workCh {
					token := tokens[idx]

					subtreeIdxs := evt.TokenSubtreeIdx.SubtreeIndices(token)
					if len(subtreeIdxs) == 0 {
						continue
					}

					positions = positions[:0]
					for k := range subtreeMap {
						delete(subtreeMap, k)
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
						subtreeMap[si] = sc
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

					bumpBytes, err := assembleTokenBUMPFast(
						blockHeight, positions, subtreeMap, topProofs,
						subtreeHeight, topTreeHeight, totalHeight,
						ws,
					)
					if err != nil {
						slog.Error("BUMP build failed", "token", token, "err", err)
						continue
					}
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
