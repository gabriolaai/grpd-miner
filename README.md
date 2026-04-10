# grpd-miner

A standalone proof-of-work miner for the Gabriola Republic Dollar (GRPD) blockchain.

The miner has no access to wallet keys and no knowledge of account balances. It only needs a GRPD address to send block rewards to. You obtain that address from the GRPD Wallet app or the ledger web interface — the miner itself never handles private keys.

---

## Requirements

- Go 1.22 or later (to build from source)
- At least 128 MB of free RAM **per CPU core** used for mining (e.g. 1 GB for an 8-core machine)
- A reliable internet connection to a GRPD node

---

## Quick Start

```
grpd-miner -reward <your-64-character-address>
```

The miner will:
1. Fetch the current peer list from `peers.txt`
2. Version-check each peer and pick one at random
3. Fetch a candidate block
4. Grind nonces until a valid hash is found
5. Submit the solved block and collect the reward
6. Loop immediately to the next block

Press **Ctrl-C** to stop. A summary of blocks mined and elapsed time is printed on exit.

---

## Flags

| Flag | Default | Description |
|---|---|---|
| `-reward` | *(required)* | Your 64-character GRPD reward address |
| `-node` | `https://grpd.gabriolarepublic.art` | Fallback node URL if peers.txt is unavailable |
| `-peers` | `https://dollar.gabriolarepublic.art/peers.txt` | URL of the peer list file |

---

## Building from Source

```bash
git clone <repo>
cd grpd-miner
go build -o grpd-miner .
```

### Cross-compilation

Go cross-compiles cleanly with no CGO dependencies:

```bash
# Windows (64-bit)
GOOS=windows GOARCH=amd64 go build -o grpd-miner.exe .

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o grpd-miner-mac-m1 .

# macOS (Intel)
GOOS=darwin GOARCH=amd64 go build -o grpd-miner-mac .

# Linux (64-bit)
GOOS=linux GOARCH=amd64 go build -o grpd-miner-linux .
```

The only external dependency is `golang.org/x/crypto` (the Go team's extended cryptography library). Run `go mod download` before cross-compiling if the module cache is empty.

---

## How Mining Works

### Proof-of-Work Algorithm

GRPD uses **Argon2id** as its proof-of-work hash function. Argon2id is a memory-hard algorithm designed to keep mining competitive on ordinary CPU hardware by making specialised mining equipment (ASICs) economically impractical and reducing the GPU advantage to roughly 2–5× over a laptop.

Parameters (consensus-critical — must match the node exactly):

| Parameter | Value |
|---|---|
| Algorithm | Argon2id |
| Memory | 131072 KB (128 MB) |
| Iterations | 1 |
| Parallelism | 1 thread per hash |
| Output length | 32 bytes (64 hex chars) |
| Salt | `gabriola-republic-dollar-v3` |

### Block Hash Construction

For each nonce attempt, the miner constructs this input string:

```
"{index}{previousHash}{timestamp}{txRoot}{nonce}"
```

- `index` — block sequence number (integer, no padding)
- `previousHash` — 64-character hex hash of the preceding block
- `timestamp` — Unix seconds with millisecond precision; whole-number timestamps have **no decimal point** (e.g. `1775606400` not `1775606400.0`)
- `txRoot` — `SHA-256(tx[0].hash + tx[1].hash + …)` — a single hash committing to all transactions without including full ML-DSA44 signatures in the PoW input
- `nonce` — 64-bit signed integer, incremented per attempt

The Argon2id hash of this string is computed with the parameters above. The result is a 64-character hex string.

### Difficulty Check

A hash meets the difficulty target when:

```
uint64(hash[0:14], base=16) < difficulty
```

The first 14 hex characters of the hash are interpreted as an unsigned 56-bit integer. A lower value represents more work. The node provides the current `difficulty` value in the candidate response; the miner compares the hash against it after every Argon2id computation.

### Multi-Core Mining

The miner spawns one goroutine per CPU core. Each goroutine starts at a different nonce offset and advances by `numCPU` per iteration, so nonces are partitioned without overlap. The first goroutine to find a valid hash signals all others to stop via a shared channel.

Memory usage scales with core count: 128 MB × number of cores. An 8-core machine uses approximately 1 GB during mining.

### Transaction Root

Full GRPD transactions include ML-DSA44 post-quantum signatures (~2.4 KB per input). To keep PoW cost independent of transaction size, the block hash does not include raw transaction data. Instead, `txRoot` is a SHA-256 of the concatenated transaction hashes:

```
txRoot = SHA-256(tx[0].hash + tx[1].hash + … + tx[n].hash)
```

Each `tx.hash` is itself a SHA-256 that commits to the full transaction content. This is verified by the node — the miner reads transaction hashes from the candidate response and does not need to parse or verify signatures.

---

## Node API

The miner uses two unauthenticated endpoints — no API key or token is required.

### Fetch candidate

```
GET /miner/candidate?rewardAddress=<address>
```

Response:
```json
{
  "block": {
    "index": 42,
    "previousHash": "a3f9...",
    "timestamp": 1775606400,
    "nonce": 0,
    "transactions": [
      {"hash": "b1c2..."},
      {"hash": "d3e4..."}
    ],
    "hash": ""
  },
  "difficulty": 75000000000000
}
```

The node pre-assembles the candidate block (including reward and fee transactions) and sets `rewardAddress` as the beneficiary of the 1 GRPD block reward.

### Submit solved block

```
POST /miner/submit
Content-Type: application/json

{ <solved block with valid nonce and hash> }
```

Returns HTTP 200 or 201 on acceptance. Returns an error if the block is rejected (e.g. another miner submitted first — this is normal and the miner simply fetches the next candidate).

---

## Peer Discovery and Failover

On startup the miner fetches `peers.txt` — a newline-delimited list of node URLs:

```
# GRPD peer list
https://grpd.gabriolarepublic.art
https://node2.example.com
```

Lines beginning with `#` are comments. The list is shuffled randomly so load is distributed across peers over time. Each peer is version-checked against the compiled-in `protocolVersion` before use — a node running a different protocol version is skipped.

If the active peer fails during a fetch or submit, the miner advances to the next peer in the shuffled list. If all peers fail, it waits 10 seconds, reshuffles, and retries. The `-node` flag provides a single fallback URL used only when `peers.txt` cannot be fetched.

---

## Protocol Version

The miner has a compiled-in `protocolVersion` constant (currently `3`). On startup it calls `GET /node/version` on each candidate peer and compares:

```json
{"protocolVersion": 3}
```

If the node reports a different version the miner skips that peer. If no compatible peer is found it exits with an error. This prevents wasted work when the network upgrades and nodes are restarted incrementally.

When a new protocol version is released, rebuild the miner from the updated source to get the matching constant.

---

## Example Output

```
GRPD Miner
  peers:    2 known (https://dollar.gabriolarepublic.art/peers.txt)
  reward:   a3f9c1...8d2e
  workers:  8 CPU cores

[14:02:11] mining block #103  difficulty=75000000000000  peer=https://grpd.gabriolarepublic.art
[14:04:38] block #103 accepted  nonce=14  hash=00000a3f…c1d28e4b
[14:04:38] mining block #104  difficulty=74812000000000  peer=https://grpd.gabriolarepublic.art
^C
stopped — 1 block(s) mined, 0 error(s), elapsed 2m27s
```

Each Argon2id hash takes approximately 1–3 seconds per core depending on hardware. Block times average 2 minutes at the network difficulty target.

---

## Tips

**RAM is the main constraint.** If your machine runs out of memory during mining, reduce the number of active cores by running the miner on a machine with more RAM, or accept that some cores will be memory-constrained and slower.

**Race losses are normal.** If another miner submits a block while you are computing, the node will reject your submission with an error. The miner logs this and immediately fetches the next candidate. No work is wasted beyond the time spent on that block.

**Running multiple instances** on the same machine does not help — they compete for the same CPU cores and RAM. Run one instance and let the multi-core worker pool use all available cores.

**Leaving it running overnight** on a laptop is fine. The miner responds immediately to Ctrl-C and prints a clean summary.
