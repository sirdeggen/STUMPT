package merkleservice

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	gosdk "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
	"github.com/bsv-blockchain/stumpt/internal/subtree"
)

// processBUMPs is the entry-point for block-finalization BUMP work.
// It builds compound BUMPs for all 100 tokens and delivers them via HTTP.
func processBUMPs(
	ctx context.Context,
	blockHeight uint32,
	subtreeHeight, topTreeHeight, totalHeight int,
	mc *metrics.Collector,
	evt *BlockFinalizedEvent,
	callbackAddr string,
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

	// Assemble a compound BUMP for each token.
	t1 := time.Now()
	type result struct {
		token string
		cb    CallbackInfo
		data  []byte
	}
	results := make([]result, 0, len(evt.Callbacks))

	for token, cb := range evt.Callbacks {
		proofs := evt.TokenProofs[token]
		if len(proofs) == 0 {
			slog.Warn("no proofs for token, skipping", "token", token)
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

		results = append(results, result{token: token, cb: cb, data: bump.Bytes()})
	}

	mc.RecordBUMPAssembly(time.Since(t1))
	slog.Info("BUMPs assembled",
		"tokens", len(results),
		"assemblyDuration", time.Since(t1),
	)

	// Deliver each BUMP to the callback URL.
	client := &http.Client{Timeout: 15 * time.Second}
	blockTime := time.Now()
	for _, r := range results {
		deliverBUMP(ctx, client, mc, r.cb, r.token, r.data, blockTime)
	}
}

// buildCompoundBUMP creates a single MerklePath that covers all of a token's
// txids in the block by combining individual per-txid BUMPs.
func buildCompoundBUMP(
	blockHeight uint32,
	proofs []*SubtreeProof,
	topProofs [][]*chainhash.Hash, // topProofs[subtreeIdx] = top-tree sibling path
	subtreeHeight, topTreeHeight, totalHeight int,
) (*gosdk.MerklePath, error) {
	var combined *gosdk.MerklePath

	for _, proof := range proofs {
		single, err := buildSingleBUMP(blockHeight, proof, topProofs, subtreeHeight, topTreeHeight, totalHeight)
		if err != nil {
			return nil, fmt.Errorf("buildSingle: %w", err)
		}
		if combined == nil {
			combined = single
			continue
		}
		if err := combined.Combine(single); err != nil {
			return nil, fmt.Errorf("combine: %w", err)
		}
	}

	return combined, nil
}

// buildSingleBUMP creates a full-block-height BUMP for one txid.
//
// BUMP level layout:
//
//	levels  0 … subtreeHeight-1  : subtree-internal sibling hashes
//	levels  subtreeHeight … totalHeight-1 : top-tree sibling hashes
//
// At BUMP level k the sibling's block-level offset is (globalLeafOffset >> k) ^ 1.
func buildSingleBUMP(
	blockHeight uint32,
	proof *SubtreeProof,
	topProofs [][]*chainhash.Hash,
	subtreeHeight, topTreeHeight, totalHeight int,
) (*gosdk.MerklePath, error) {
	if len(proof.SiblingPath) < subtreeHeight {
		return nil, fmt.Errorf("subtree proof too short: got %d want %d",
			len(proof.SiblingPath), subtreeHeight)
	}
	si := proof.SubtreeIdx
	if si >= len(topProofs) || len(topProofs[si]) < topTreeHeight {
		return nil, fmt.Errorf("top-tree proof missing for subtree %d", si)
	}

	g := uint64(proof.GlobalIdx) //nolint:gosec

	path := make([][]*gosdk.PathElement, totalHeight)

	// ── Level 0: txid itself + its leaf sibling ──────────────────────────────
	txidHash := proof.TxID
	sibHash0 := proof.SiblingPath[0]
	path[0] = []*gosdk.PathElement{
		{Offset: g, Hash: &txidHash, Txid: boolPtr(true)},
		{Offset: g ^ 1, Hash: sibHash0},
	}

	// ── Subtree levels 1 … subtreeHeight-1 ──────────────────────────────────
	for k := 1; k < subtreeHeight; k++ {
		sibOffset := (g >> k) ^ 1
		path[k] = []*gosdk.PathElement{
			{Offset: sibOffset, Hash: proof.SiblingPath[k]},
		}
	}

	// ── Top-tree levels subtreeHeight … totalHeight-1 ────────────────────────
	tp := topProofs[si]
	for k := 0; k < topTreeHeight; k++ {
		blockLevel := subtreeHeight + k
		sibOffset := (g >> blockLevel) ^ 1
		path[blockLevel] = []*gosdk.PathElement{
			{Offset: sibOffset, Hash: tp[k]},
		}
	}

	return gosdk.NewMerklePath(blockHeight, path), nil
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

// boolPtr returns a pointer to a bool literal.
func boolPtr(b bool) *bool { return &b }
