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
- Submits them at a rate of `HASHES_PER_BLOCK / 600` per second so the full block is generated over 10 minutes.
- Each submission randomly picks one of 100 callback tokens, simulating 100 distinct submitting businesses.

### Merkle Service (`internal/merkleservice`)
- Receives `POST /watch` with `{ txid, callback: { url, token } }`.
- Every `HASHES_PER_SUBTREE` txids received, **seals a subtree**:
  - Builds 3 miners' versions of the subtree with deterministically jittered ordering (miner 0 = canonical, miner 1 = 5 % adjacent swaps, miner 2 = 10 %).
  - Pre-computes miner-0 merkle proofs for every token that has txids in the subtree.
- When `HASHES_PER_BLOCK` txids are received, **finalises the block**:
  - Builds the top tree from the 60 subtree roots (one call to `GetAllProofs`).
  - For each of the 100 tokens, combines all pre-computed subtree proofs with the top-tree legs into a single compound `MerklePath` using go-sdk's `Combine`.
  - POSTs the raw BUMP binary to each token's callback URL.

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
| `HASHES_PER_BLOCK` | 61 440 | 60 subtrees × 1 024 |
| `HASHES_PER_SUBTREE` | 1 024 | subtree height = 10 |
| Subtrees per block | 60 | top tree height = 6 |
| Block merkle height | 16 | 10 + 6 |
| Num miners | 3 | competing subtree orderings |
| Num businesses | 100 | distinct callback tokens |
| Test duration | 10 min | ≈ 102.4 txids/sec |
| Mock block height | 800 000 | stamped in every BUMP |

---

## Prerequisites

- Go 1.24 or later (`go version`)
- Ports **:8080** (merkle service) and **:3000** (callback server) available

---

## Running the test

### 1. Clone and build

```bash
git clone https://github.com/bsv-blockchain/stumpt
cd stumpt
go build -o harness ./cmd/harness/
```

### 2. Run with default parameters (10-minute full test)

```bash
./harness
```

The harness starts both servers, then the generator.  Structured JSON logs stream to stdout.  When all txids have been submitted and all BUMPs delivered, a summary table is printed:

```
╔══════════════════════════════════════════╗
║          STUMPT FINAL SUMMARY            ║
╠══════════════════════════════════════════╣
║  Elapsed:                       10m0.3s  ║
║  Txids submitted:               61440    ║
║  Actual rate:                  102.4/s   ║
╠══════════════════════════════════════════╣
║  Subtrees sealed:                   60   ║
║  Avg seal time:                  8.50ms  ║
║  Proof pre-computations:            60   ║
║  Avg proof time:                45.00ms  ║
╠══════════════════════════════════════════╣
║  Top tree build:                 1.00ms  ║
║  BUMP assembly (100tok):       250.00ms  ║
║  Callbacks delivered:              100   ║
║  Avg callback time:              5.00ms  ║
║  Avg BUMP size:                8000 B    ║
║  Total BUMP bytes:            800000 B   ║
╚══════════════════════════════════════════╝
```

### 3. Quick smoke test (completes in ~10 seconds)

```bash
./harness \
  -hashes-per-block 1024 \
  -hashes-per-subtree 64
```

This runs 1 024 txids at 1.7 txids/sec across 16 subtrees and exercises the full pipeline in about 10 seconds.

### 4. All CLI flags

```
-hashes-per-block   int    Total txids per simulated block    (default 61440)
-hashes-per-subtree int    Txids per subtree                  (default 1024)
-miners             int    Number of competing miners         (default 3)
-merkle-addr        string Merkle service listen address      (default :8080)
-callback-addr      string Callback server listen address     (default :3000)
```

`hashes-per-block` must be divisible by `hashes-per-subtree`.

---

## Running the tests

The merkle engine has a full unit + integration test suite that verifies proof correctness against go-sdk's `MerklePath.ComputeRoot` and `Combine`.

```bash
go test ./...
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

- **During the block interval (10 min):** subtree sealing + proof pre-computation runs continuously. Each subtree seal takes `~seal_time × num_miners` ms and pre-computing proofs takes `~proof_time` ms per subtree. This work is spread over 60 subtree boundaries across 10 minutes.
- **At block found:** only the top-tree build (`~1 ms` for 60 nodes) and BUMP assembly (`~Xms` for 100 tokens) need to happen before delivery. The pre-computed proofs pay for themselves here.
- **Per-business BUMP size** reflects how many txids that business submitted. Businesses with more txids get larger but more compact compound BUMPs (intermediate hashes are pruned by `Combine`).

---

## Dependencies

| Package | Use |
|---------|-----|
| `github.com/bsv-blockchain/go-sdk` | `transaction.MerklePath`, `PathElement`, `Combine`, `chainhash.Hash` |

The merkle tree implementation (`internal/subtree`) uses only Go's standard `crypto/sha256` and the go-sdk `chainhash` type.  There is no dependency on `go-bt` or `go-subtree`.
