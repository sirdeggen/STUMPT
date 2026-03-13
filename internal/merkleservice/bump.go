package merkleservice

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
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

// processBUMPs is the entry-point for block-finalization BUMP work.
// It builds compound BUMPs for all tokens using a parallel worker pool
// and delivers them via HTTP.
// If dumpFile is non-empty the first assembled BUMP is written as a hex string.
func processBUMPs(
	ctx context.Context,
	blockHeight uint32,
	subtreeHeight, topTreeHeight, totalHeight int,
	mc *metrics.Collector,
	evt *BlockFinalizedEvent,
	callbackAddr string,
	dumpFile string,
) {
	t0 := time.Now()

	// Pre-compute top-tree proofs for every subtree index once.
	// GetAllProofs builds the merkle store once and returns all n proofs.
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

	// Assemble and deliver BUMPs in a streaming pipeline.
	// Workers build BUMPs and immediately hand them off for delivery,
	// avoiding accumulating all BUMP byte slices in memory at once.
	t1 := time.Now()
	type tokenWork struct {
		token string
		cb    CallbackInfo
	}
	work := make([]tokenWork, 0, len(evt.Callbacks))
	for token, cb := range evt.Callbacks {
		work = append(work, tokenWork{token: token, cb: cb})
	}

	total := len(work)
	// Each worker holds one BUMP's worth of maps + serialized bytes in flight.
	// With streaming delivery, completed BUMPs are freed immediately.
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
	)

	// Pre-load all miner-0 subtree stores into memory for fast BUMP assembly.
	// At 600 subtrees × ~64MB each this is ~38GB — may need to be bounded.
	type subtreeCache struct {
		Leaves []chainhash.Hash
		Store  []chainhash.Hash
	}
	numSubtrees := len(evt.SubtreeRoots)
	stCache := make([]subtreeCache, numSubtrees)
	{
		var cacheWg sync.WaitGroup
		cacheWg.Add(numSubtrees)
		for si := 0; si < numSubtrees; si++ {
			go func(si int) {
				defer cacheWg.Done()
				leavesBytes, storeBytes, ok := evt.MinerSubStore.Load(0, si)
				if !ok {
					slog.Error("BUMP cache: failed to load subtree", "subtreeIdx", si)
					return
				}
				stCache[si] = subtreeCache{
					Leaves: bytesToHashSlice(leavesBytes),
					Store:  bytesToHashSlice(storeBytes),
				}
			}(si)
		}
		cacheWg.Wait()
		slog.Info("subtree cache loaded",
			"subtrees", numSubtrees,
			"estMemoryMB", numSubtrees*64,
		)
	}

	type deliveryItem struct {
		token string
		cb    CallbackInfo
		data  []byte
	}

	// Streaming pipeline: build workers → delivery channel → delivery workers.
	deliverCh := make(chan deliveryItem, numWorkers*2)
	workCh := make(chan int, total)

	// Track progress atomically.
	var doneCount sync.Mutex
	done := 0
	dumped := false

	client := &http.Client{Timeout: 15 * time.Second}
	blockTime := time.Now()

	// Start delivery workers first.
	deliveryWorkers := numWorkers
	if deliveryWorkers > 32 {
		deliveryWorkers = 32
	}
	var deliverWg sync.WaitGroup
	deliverWg.Add(deliveryWorkers)
	for w := 0; w < deliveryWorkers; w++ {
		go func() {
			defer deliverWg.Done()
			for item := range deliverCh {
				deliverBUMP(ctx, client, mc, item.cb, item.token, item.data, blockTime)
			}
		}()
	}

	// Launch build workers.
	var buildWg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		buildWg.Add(1)
		go func() {
			defer buildWg.Done()
			for idx := range workCh {
				tw := work[idx]

				// On-demand proof computation from cached subtree stores.
				var proofs []*SubtreeProof
				subtreeIdxs := evt.TokenSubtreeIdx.SubtreeIndices(tw.token)
				for _, si := range subtreeIdxs {
					localIdxs := evt.TokenSubtreeIdx.Get(tw.token, si)
					if len(localIdxs) == 0 || si >= len(stCache) {
						continue
					}
					sc := &stCache[si]
					if sc.Leaves == nil {
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
					slog.Error("BUMP build failed", "token", tw.token, "err", err)
					continue
				}
				bumpBytes := bump.Bytes()

				// Dump first BUMP if requested.
				if dumpFile != "" {
					doneCount.Lock()
					if !dumped {
						dumped = true
						doneCount.Unlock()
						dumpBUMPHex(dumpFile, tw.token, bumpBytes)
					} else {
						doneCount.Unlock()
					}
				}

				// Stream to delivery immediately — don't accumulate.
				deliverCh <- deliveryItem{token: tw.token, cb: tw.cb, data: bumpBytes}

				// Log progress every 10%.
				doneCount.Lock()
				done++
				d := done
				doneCount.Unlock()
				if total >= 10 && d%(total/10) == 0 {
					slog.Info("BUMP assembly progress",
						"done", d,
						"total", total,
						"elapsed", time.Since(t1).Round(time.Millisecond),
					)
				}
			}
		}()
	}

	// Enqueue work.
	for i := range work {
		workCh <- i
	}
	close(workCh)
	buildWg.Wait()
	close(deliverCh)

	mc.RecordBUMPAssembly(time.Since(t1))
	slog.Info("BUMPs assembled",
		"tokens", total,
		"workers", numWorkers,
		"assemblyDuration", time.Since(t1),
	)

	deliverWg.Wait()
}

// buildCompoundBUMP creates a single MerklePath that covers all of a token's
// txids in one O(n·h) pass — no repeated Combine calls.
//
// Strategy:
//  1. Accumulate every PathElement from every per-txid proof into a
//     map[level]map[offset]*PathElement.  Because all proofs share the same
//     block tree, duplicate offsets at the same level always carry the same
//     hash, so last-write is fine.
//  2. Prune intermediate nodes whose both children are already present
//     (identical to what Combine does internally, but only once).
//  3. Sort each level by offset and wrap in a MerklePath.
func buildCompoundBUMP(
	blockHeight uint32,
	proofs []*SubtreeProof,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
) (*gosdk.MerklePath, error) {
	if len(proofs) == 0 {
		return nil, fmt.Errorf("buildCompoundBUMP: no proofs")
	}

	// merged[level][offset] = PathElement
	// Pre-allocate with estimated capacity: level 0 has ~2× proofs (txid + sibling),
	// higher levels have ~1 entry per proof (siblings deduplicate).
	numProofs := len(proofs)
	merged := make([]map[uint64]*gosdk.PathElement, totalHeight)
	merged[0] = make(map[uint64]*gosdk.PathElement, numProofs*2)
	for i := 1; i < totalHeight; i++ {
		merged[i] = make(map[uint64]*gosdk.PathElement, numProofs)
	}

	// Accumulate all elements from every per-txid proof.
	for _, proof := range proofs {
		if err := accumulateProof(proof, topProofs, subtreeHeight, topTreeHeight, totalHeight, merged); err != nil {
			return nil, err
		}
	}

	// Prune: remove a node at level h if both its children (at h-1) are
	// present — it can be recomputed and is redundant in the BUMP.
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

	// Build the final [][]*PathElement, sorted by offset per level.
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

	// Level 0: txid itself + leaf sibling.
	txidHash := proof.TxID
	merged[0][g] = &gosdk.PathElement{Offset: g, Hash: &txidHash, Txid: boolPtr(true)}
	merged[0][g^1] = &gosdk.PathElement{Offset: g ^ 1, Hash: proof.SiblingPath[0]}

	// Subtree levels 1 … subtreeHeight-1.
	for k := 1; k < subtreeHeight; k++ {
		sibOffset := (g >> k) ^ 1
		merged[k][sibOffset] = &gosdk.PathElement{Offset: sibOffset, Hash: proof.SiblingPath[k]}
	}

	// Top-tree levels subtreeHeight … totalHeight-1.
	tp := topProofs[si]
	for k := 0; k < topTreeHeight; k++ {
		blockLevel := subtreeHeight + k
		sibOffset := (g >> blockLevel) ^ 1
		merged[blockLevel][sibOffset] = &gosdk.PathElement{Offset: sibOffset, Hash: tp[k]}
	}

	return nil
}

// deliverBUMP HTTP-POSTs raw BUMP bytes to the callback URL.
func deliverBUMP(
	ctx context.Context,
	client *http.Client,
	mc *metrics.Collector,
	cb CallbackInfo,
	token string,
	data []byte,
	blockTime time.Time,
) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cb.URL, bytes.NewReader(data))
	if err != nil {
		slog.Error("callback: build request", "token", token, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Callback-Token", token)

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("callback: POST failed", "token", token, "err", err)
		return
	}
	_ = resp.Body.Close()

	latency := time.Since(blockTime)
	mc.RecordCallback(latency, len(data))

	slog.Info("BUMP delivered",
		"token", token,
		"bytes", len(data),
		"httpMs", time.Since(t0).Milliseconds(),
		"blockLatency", latency,
	)
}

// dumpBUMPHex writes the BUMP binary as a UTF-8 hex string to path.
// The file contains exactly one line: the lowercase hex-encoded BUMP bytes.
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
