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

	// Assemble compound BUMPs using a parallel worker pool.
	// BUMP assembly is embarrassingly parallel: each token's BUMP is
	// independent. We use min(GOMAXPROCS, numTokens) workers.
	t1 := time.Now()
	type result struct {
		token string
		cb    CallbackInfo
		data  []byte
	}

	// Collect all tokens into a slice for deterministic ordering.
	type tokenWork struct {
		token  string
		cb     CallbackInfo
		proofs []*SubtreeProof
	}
	work := make([]tokenWork, 0, len(evt.Callbacks))
	for token, cb := range evt.Callbacks {
		proofs := evt.TokenProofs[token]
		if len(proofs) == 0 {
			slog.Warn("no proofs for token, skipping", "token", token)
			continue
		}
		work = append(work, tokenWork{token: token, cb: cb, proofs: proofs})
	}

	total := len(work)
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

	results := make([]result, total)
	var wg sync.WaitGroup
	workCh := make(chan int, total)

	// Track progress atomically.
	var doneCount sync.Mutex
	done := 0

	// Launch workers.
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range workCh {
				tw := work[idx]
				bump, err := buildCompoundBUMP(
					blockHeight, tw.proofs, topProofs,
					subtreeHeight, topTreeHeight, totalHeight,
				)
				if err != nil {
					slog.Error("BUMP build failed", "token", tw.token, "err", err)
					continue
				}
				bumpBytes := bump.Bytes()
				results[idx] = result{token: tw.token, cb: tw.cb, data: bumpBytes}

				// Log progress every 10% for large token counts.
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
	wg.Wait()

	mc.RecordBUMPAssembly(time.Since(t1))
	slog.Info("BUMPs assembled",
		"tokens", total,
		"workers", numWorkers,
		"assemblyDuration", time.Since(t1),
	)

	// Dump the first non-empty BUMP to file as hex if requested.
	if dumpFile != "" {
		for _, r := range results {
			if len(r.data) > 0 {
				dumpBUMPHex(dumpFile, r.token, r.data)
				break
			}
		}
	}

	// Deliver each BUMP to the callback URL.
	client := &http.Client{Timeout: 15 * time.Second}
	blockTime := time.Now()
	for _, r := range results {
		if len(r.data) == 0 {
			continue
		}
		deliverBUMP(ctx, client, mc, r.cb, r.token, r.data, blockTime)
	}
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
	merged := make([]map[uint64]*gosdk.PathElement, totalHeight)
	for i := range merged {
		merged[i] = make(map[uint64]*gosdk.PathElement)
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
