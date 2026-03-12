# STUMPT

**S**ubtree + **T**imed **U**tility for **M**erkle **P**ath **T**esting

A performance test harness for the theory behind incremental compound BUMP (BRC-74) generation at block-scale transaction volumes.

---

## What is this testing?

Bitcoin SV blocks at Teranode scale contain millions of transactions.  When a block is found, every business that submitted transactions needs to receive a **BUMP** (Bitcoin UTXO Merkle Path) — a compact proof that their transactions are included in the block.

The naive approach is to build the full block merkle tree at the moment the block is found and then compute proofs for all subscribers.  At 600 million transactions per block that is not feasible in the time available.

The theory being tested here is a **subtree-based incremental approach**:

1. As transactions arrive over the ~10 minutes between blocks, group them into fixed-size **subtrees**.
2. Each subtree's internal merkle path can be pre-computed as soon as the subtree is sealed — spreading the work evenly across the inter-block interval.
3. When the block is found, a small **top tree** (one leaf per subtree root) is built and the top-level proof legs are appended to the pre-computed subtree paths.
4. Each subscribing business receives a single **compound BUMP** that covers all of their transactions at once.

This repo simulates that pipeline end-to-end and measures the timing of each phase.

---

## Architecture

Three components run in a single process, communicating over localhost HTTP:

```
  Generator ──POST /watch──► Merkle Service ──POST (raw BUMP)──► Callback Server
```

### Generator (`internal/generator`)
- Produces `HASHES_PER_BLOCK` random 32-byte hashes (mock txids).
- Submits them at a rate of `HASHES_PER_BLOCK / duration` per second.
- Each submission picks one of 100 callback tokens round-robin, simulating 100 distinct submitting businesses.

### Merkle Service (`internal/merkleservice`)
- Receives `POST /watch` with `{ txid, callback: { url, token } }`.
- Every `HASHES_PER_SUBTREE` txids received, **seals a subtree**:
  - Builds 3 miners' versions of the subtree with deterministically jittered ordering (miner 0 = canonical, miner 1 = 5 % adjacent swaps, miner 2 = 10 %).
  - Pre-computes miner-0 merkle proofs for every token that has txids in the subtree.
- When `HASHES_PER_BLOCK` txids are received, **finalises the block**:
  - Builds the top tree from all subtree roots (one call to `GetAllProofs`).
  - For each of the 100 tokens, combines all pre-computed subtree proofs with the top-tree legs into a single compound `MerklePath` using go-sdk's `Combine`.
  - POSTs the raw BUMP binary to each token's callback URL.
  - Signals the main process to exit cleanly.

### Callback Server (`internal/callback`)
- Receives the raw BUMP binary POSTs.
- Logs token, payload size, and end-to-end latency from block announcement to delivery.

### Subtree / Merkle Engine (`internal/subtree`)
- Implements `BuildMerkleStore`, `GetMerkleProof`, and `GetAllProofs` using `go-sdk/chainhash` only — no dependency on `go-bt` or `go-subtree`.
- `HashPair` is byte-identical to `transaction.MerkleTreeParent` in go-sdk, verified by test.

---

## Default configuration

| Parameter | Default | Notes |
|-----------|---------|-------|
| `HASHES_PER_BLOCK` | 1 024 | 16 subtrees × 64 |
| `HASHES_PER_SUBTREE` | 64 | subtree height = 6 |
| Subtrees per block | 16 | top tree height = 4 |
| Block merkle height | 10 | 6 + 4 |
| Num miners | 3 | competing subtree orderings |
| Num businesses | 100 | distinct callback tokens |
| Test duration | 10 s | ≈ 102.4 txids/sec |
| Mock block height | 800 000 | stamped in every BUMP |

---

## Prerequisites

- Go 1.24 or later (`go version`)
- Ports **:18080** (merkle service) and **:13000** (callback server) available

---

## Running the test

### 1. Clone and build

```bash
git clone https://github.com/bsv-blockchain/stumpt
cd stumpt
go build -o harness ./cmd/harness/
```

### 2. Run with default parameters (~10 seconds)

```bash
./harness
```

The harness starts both servers, then the generator.  Structured JSON logs stream to stdout.  When all txids have been submitted and all BUMPs delivered, a summary table is printed and the process exits:

```
╔══════════════════════════════════════════╗
║          STUMPT FINAL SUMMARY            ║
╠══════════════════════════════════════════╣
║  Elapsed:                        10.183s ║
║  Txids submitted:                   1024 ║
║  Actual rate:                   102.4/s  ║
╠══════════════════════════════════════════╣
║  Subtrees sealed:                    16  ║
║  Avg seal time:                  0.08ms  ║
║  Proof pre-computations:             16  ║
║  Avg proof time:                 0.05ms  ║
╠══════════════════════════════════════════╣
║  Top tree build:                 0.00ms  ║
║  BUMP assembly (100tok):         1.46ms  ║
║  Callbacks delivered:               200  ║
║  Avg callback time:              5.22ms  ║
║  Avg BUMP size:                   759 B  ║
║  Total BUMP bytes:             151880 B  ║
╚══════════════════════════════════════════╝
```

### 3. Full-scale test (10-minute block simulation)

```bash
./harness \
  -hashes-per-block 61440 \
  -hashes-per-subtree 1024 \
  -duration 10m
```

This runs 61 440 txids (60 subtrees × 1 024) at ~102.4 txids/sec over 10 minutes, matching a real Teranode block interval.

### 4. All CLI flags

```
-hashes-per-block   int      Total txids per simulated block    (default 1024)
-hashes-per-subtree int      Txids per subtree                  (default 64)
-miners             int      Number of competing miners         (default 3)
-businesses         int      Distinct callback tokens           (default 100)
-duration           duration Total test duration                (default 10s)
-merkle-addr        string   Merkle service listen address      (default :18080)
-callback-addr      string   Callback server listen address     (default :13000)
-dump-bump          string   Write first assembled BUMP as hex to this file (optional)
```

`hashes-per-block` must be divisible by `hashes-per-subtree`.

**To simulate one BUMP per txid** (each token registers exactly one transaction), set `-businesses` equal to `-hashes-per-block`:

```bash
./harness -businesses 1024
```

**To simulate one compound BUMP per business** across many txids, keep `-businesses` well below `-hashes-per-block` (the default 100 gives ~10 txids per token at block size 1024).

---

## Running the tests

The merkle engine has a full unit + integration test suite that verifies proof correctness against go-sdk's `MerklePath.ComputeRoot` and `Combine`.

```bash
go test ./...
```

With the race detector:

```bash
go test -race ./...
```

Benchmarks:

```bash
go test -bench=. -benchmem ./internal/subtree/
```

Key tests in `internal/subtree/`:

| Test | What it checks |
|------|----------------|
| `TestHashPairMatchesGoSDK` | `HashPair` is byte-identical to `transaction.MerkleTreeParent` |
| `TestProofRoundTripSmall` | Proofs for n = 1, 2, 3, 4, 7, 8 leaves all compute back to the correct root |
| `TestProofRoundTrip1024` | Every 64th leaf of a 1 024-leaf subtree verifies correctly |
| `TestCompoundBUMPCombine` | Two per-txid BUMPs combine and both txids still verify |
| `TestGetAllProofs` | Batch proof generation matches single-proof generation |
| `TestCompoundBUMPAcrossSubtrees` | 4 subtrees × 4 leaves: individual + compound BUMPs all verify against the block root |
| `TestCompoundBUMPDefaultConfig` | 61 440-txid / 60-subtree / 16-level tree: sample of 10 txids compounded and verified |

---

## What the numbers tell you

The key insight this harness measures is the **work distribution**:

- **During the block interval:** subtree sealing + proof pre-computation runs continuously. Each subtree seal takes `~seal_time × num_miners` ms and pre-computing proofs takes `~proof_time` ms per subtree. This work is spread evenly across all subtree boundaries throughout the inter-block interval.
- **At block found:** only the top-tree build (microseconds for ≤64 subtree roots) and BUMP assembly (~ms for 100 tokens) need to happen before delivery. The pre-computed proofs pay for themselves here.
- **Per-business BUMP size** reflects how many txids that business submitted. Businesses with more txids get larger but more compact compound BUMPs (intermediate hashes are pruned by `Combine`).

---

## Dependencies

| Package | Use |
|---------|-----|
| `github.com/bsv-blockchain/go-sdk` | `transaction.MerklePath`, `PathElement`, `Combine`, `chainhash.Hash` |

The merkle tree implementation (`internal/subtree`) uses only Go's standard `crypto/sha256` and the go-sdk `chainhash` type.  There is no dependency on `go-bt` or `go-subtree`.
