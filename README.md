# STUMPT

**S**ubTree Unified **M**erkle **P**ath **T**esting

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

Three components run in a single process:

```
  Generator ──POST /watch──► Merkle Service ──POST (raw BUMP)──► Callback Server
```

In **direct mode** (`-direct` flag), the generator bypasses HTTP and calls the registry directly, enabling large-scale runs (6M+ txids) that would be bottlenecked by HTTP/JSON overhead.

### Generator (`internal/generator`)
- Produces `HASHES_PER_BLOCK` random 32-byte hashes (mock txids).
- **HTTP mode:** Submits at a rate of `HASHES_PER_BLOCK / duration` per second.
- **Direct mode:** Submits as fast as possible (bypasses HTTP), achieving 120k+ txids/sec.
- Each submission picks one of N callback tokens round-robin, simulating N distinct submitting businesses.

### Merkle Service (`internal/merkleservice`)
- Receives `POST /watch` with `{ txid, callback: { url, token } }` (or direct calls in `-direct` mode).
- Every `HASHES_PER_SUBTREE` txids received, **seals a subtree**:
  - Builds N miners' versions of the subtree with deterministically jittered ordering.
  - Pre-computes miner-0 merkle proofs using **STUMP XOR indexing** (see below).
- When `HASHES_PER_BLOCK` txids are received, **finalises the block**:
  - Performs coinbase replacement + subtree-0 reseal.
  - Uses **STUMP Discover** to gather proofs for all tokens via XOR probing.
  - Builds compound BUMPs in parallel using a worker pool.
  - POSTs the raw BUMP binary to each token's callback URL.

### Callback Server (`internal/callback`)
- Receives the raw BUMP binary POSTs.
- Logs token, payload size, and end-to-end latency from block announcement to delivery.

### Subtree / Merkle Engine (`internal/subtree`)
- Implements `BuildMerkleStore`, `GetMerkleProof`, and `GetAllProofs` using `go-sdk/chainhash` only — no dependency on `go-bt` or `go-subtree`.
- `HashPair` is byte-identical to `transaction.MerkleTreeParent` in go-sdk, verified by test.

### STUMP Index (`internal/stump`)
- XOR-based content-addressed proof index: `key = XOR(TokenHash, SubtreeRoot)`.
- Enables O(1) per-txid insertion during subtree sealing (vs O(tokens × txids) previously).
- At block announcement, O(subtrees × tokens) XOR probes discover all matching proofs.
- See **STUMP Indexing** section below for design rationale.

---

## STUMP Indexing

### The problem

At scale (600M txids/block, 100k+ businesses), the naive approach of iterating every token's txid list against every sealed subtree is O(tokens × txids/token) per subtree — prohibitively expensive.

### The solution

**STUMP** (**S**ubtree-**T**oken **U**nified **M**erkle **P**roof) uses XOR-based composite keys:

```
index_key = XOR(SHA256d(token_string), subtree_merkle_root)
```

#### Why XOR?

1. **Commutativity enables discovery:** On block announcement, a subscriber who knows their `tokenHash` can XOR it with each announced `subtreeRoot` to probe the store — without iterating all tokens or all subtrees.

2. **Uniform distribution:** Both `tokenHash` (SHA256d of the token string) and `subtreeRoot` (merkle root of random txids) are uniformly distributed 256-bit values, so their XOR is also uniform — no clustering in the hash map.

3. **Reversibility:** Given the XOR key and either operand, the other is recoverable: `subtreeRoot = key ^ tokenHash`. This enables verification and debugging.

4. **No collision risk:** The key space is 2^256; collisions are astronomically improbable even at 600M txids × 100k tokens.

#### Why not concatenation + hash?

`SHA256(token || subtreeRoot)` would work for indexing but loses the reversibility property. With XOR, a subscriber can probe the store in O(subtrees) rather than needing to enumerate all (token, subtree) pairs stored.

#### Lifecycle

**During inter-block interval (subtree sealing):**
```
For each txid in the sealed subtree:
  token := txidIndex.Lookup(txid)       // O(1)
  key   := XOR(tokenHash, subtreeRoot)  // O(1), ~8.5 ns
  store.Append(key, proof)              // O(1)
```

**At block announcement (STUMP Discovery):**
```
For each subtreeRoot in the winning miner's block:
  For each subscribed tokenHash:
    key := XOR(tokenHash, subtreeRoot)  // ~8.5 ns
    stumps := store.Get(key)            // O(1), ~9.5 ns
    → append to this token's proof list for BUMP assembly
```

#### Measured performance

| Operation | Time | Notes |
|-----------|------|-------|
| XOR key computation | 8.5 ns | Zero allocations |
| Store lookup | 9.5 ns | Zero allocations |
| Discover 100 subtrees × 100 tokens | 0.77 ms | 10k probes |
| Discover 6000 subtrees × 1000 tokens | 1.74 s | 6M probes |

At extreme scale (600k subtrees × 100k tokens = 60B probes), the XOR+lookup cost would be ~60B × 18 ns ≈ 18 minutes. At that scale, a sharded or per-token-parallel approach is needed — but the STUMP design supports this naturally since each token's probes are independent.

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
║  Elapsed:                          5.02s  ║
║  Txids submitted:                   1024  ║
║  Actual rate:                   204.0/s  ║
╠══════════════════════════════════════════╣
║  Subtrees sealed:                     16  ║
║  Avg seal time:                  0.05ms  ║
║  Proof pre-computations:              16  ║
║  Avg proof time:                 0.05ms  ║
╠══════════════════════════════════════════╣
║  Coinbase reseal:                0.07ms  ║
║  Top tree build:                 0.01ms  ║
║  BUMP assembly ( 100tok):        1.87ms  ║
║  Callbacks delivered:                200  ║
║  Avg callback time:              4.73ms  ║
║  Avg BUMP size:                  3038 B  ║
║  Total BUMP bytes:             607712 B  ║
╚══════════════════════════════════════════╝
```

### 3. Direct mode — large-scale runs

For runs above ~60k txids, HTTP submission becomes the bottleneck. Use `-direct` to bypass HTTP entirely:

```bash
# 6 million txids, 600 subtrees × 10k leaves, 1000 businesses
./harness -direct \
  -hashes-per-block 6000000 \
  -hashes-per-subtree 10000 \
  -businesses 1000
```

This runs 6M txids at ~125k txids/sec (limited by merkle computation, not HTTP), completing in ~47 seconds.

### 4. Full-scale test (10-minute block simulation via HTTP)

```bash
./harness \
  -hashes-per-block 61440 \
  -hashes-per-subtree 1024 \
  -duration 10m
```

This runs 61 440 txids (60 subtrees × 1 024) at ~102.4 txids/sec over 10 minutes, matching a real Teranode block interval.

### 5. All CLI flags

```
-hashes-per-block   int      Total txids per simulated block    (default 1024)
-hashes-per-subtree int      Txids per subtree                  (default 64)
-miners             int      Number of competing miners         (default 3)
-businesses         int      Distinct callback tokens           (default 100)
-duration           duration Total test duration                (default 10s; ignored in -direct mode)
-merkle-addr        string   Merkle service listen address      (default :18080)
-callback-addr      string   Callback server listen address     (default :13000)
-direct                      Bypass HTTP for txid submission    (fast path for large-scale runs)
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

The merkle engine and STUMP index have full unit + integration test suites.

```bash
go test ./...
```

With the race detector:

```bash
go test -race ./...
```

Benchmarks:

```bash
# Merkle engine benchmarks
go test -bench=. -benchmem ./internal/subtree/

# STUMP index benchmarks
go test -bench=. -benchmem ./internal/stump/

# BUMP assembly benchmarks (isolated from HTTP)
go test -bench=. -benchmem ./internal/merkleservice/
```

Key tests:

| Package | Test | What it checks |
|---------|------|----------------|
| `subtree` | `TestHashPairMatchesGoSDK` | `HashPair` is byte-identical to `transaction.MerkleTreeParent` |
| `subtree` | `TestProofRoundTripSmall` | Proofs for n = 1, 2, 3, 4, 7, 8 leaves all verify |
| `subtree` | `TestProofRoundTrip1024` | Every 64th leaf of a 1024-leaf subtree verifies |
| `subtree` | `TestCompoundBUMPCombine` | Two per-txid BUMPs combine and verify |
| `subtree` | `TestGetAllProofs` | Batch proof generation matches single-proof generation |
| `subtree` | `TestCompoundBUMPAcrossSubtrees` | 4 subtrees × 4 leaves: compound BUMPs verify against block root |
| `subtree` | `TestCompoundBUMPDefaultConfig` | 61,440-txid / 60-subtree / 16-level tree: sample compound BUMPs verify |
| `stump` | `TestXORKeySymmetry` | XOR(a, b) == XOR(b, a) |
| `stump` | `TestXORKeySelfInverse` | XOR(XOR(a, b), b) == a |
| `stump` | `TestDiscoverEndToEnd` | Full STUMP lifecycle: register, seal, discover |
| `stump` | `TestStoreAppendAndGet` | Basic store operations |
| `stump` | `TestTokenRegistry` | Token registration and hash lookup |

---

## What the numbers tell you

The key insight this harness measures is the **work distribution**:

- **During the block interval:** subtree sealing + proof pre-computation runs continuously. Each subtree seal takes `~seal_time × num_miners` ms and pre-computing proofs takes `~proof_time` ms per subtree. This work is spread evenly across all subtree boundaries throughout the inter-block interval.
- **At block found:** only the coinbase reseal (~0.05–177 ms), top-tree build (microseconds), STUMP discovery (~0.77 ms–1.74 s), and parallel BUMP assembly (~1.4 ms–18 s) need to happen before delivery. The pre-computed proofs pay for themselves here.
- **Per-business BUMP size** reflects how many txids that business submitted. Businesses with more txids get larger but more compact compound BUMPs (intermediate hashes are pruned).

---

## Dependencies

| Package | Use |
|---------|-----|
| `github.com/bsv-blockchain/go-sdk` | `transaction.MerklePath`, `PathElement`, `Combine`, `chainhash.Hash` |

The merkle tree implementation (`internal/subtree`) uses only Go's standard `crypto/sha256` and the go-sdk `chainhash` type.  There is no dependency on `go-bt` or `go-subtree`.
