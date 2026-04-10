// grpd-miner is a standalone proof-of-work miner for the GRPD blockchain.
//
// It has no dependencies beyond the Go standard library and no access to
// wallet keys — it only needs a reward address to collect mining rewards.
//
// Usage:
//
//	grpd-miner -reward <64-char address> [-node <url>]
//
// Build:
//
//	go build -o grpd-miner .
//
// Cross-compile:
//
//	GOOS=windows GOARCH=amd64 go build -o grpd-miner.exe .
//	GOOS=darwin  GOARCH=amd64 go build -o grpd-miner-mac .
//	GOOS=darwin  GOARCH=arm64 go build -o grpd-miner-mac-m1 .
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/argon2"
)

// protocolVersion must match the node. The miner refuses to work against an
// incompatible node to prevent wasted work on a chain with different rules.
const protocolVersion = 3

// defaultNode is the fallback node used when peers.txt is unavailable.
const defaultNode = "https://grpd.gabriolarepublic.art"

// defaultPeersURL is the canonical peer list for the GRPD network.
const defaultPeersURL = "https://dollar.gabriolarepublic.art/peers.txt"

// Argon2id parameters — must match config/config.go exactly.
// Changing any of these changes every block hash and breaks consensus.
const (
	argon2Memory      = uint32(131072) // 128 MB in KB
	argon2Iterations  = uint32(1)
	argon2Parallelism = uint8(1)
	argon2KeyLen      = uint32(32)
	argon2Salt        = "gabriola-republic-dollar-v3"
)

// ── block types ──────────────────────────────────────────────────────────────

// block mirrors the node's Block struct. Transactions are stored as raw JSON
// so the full transaction data (ID, type, inputs, outputs, signatures) round-trips
// to the node unchanged on submit. Only the hash field is read for txRoot.
type block struct {
	Index        int               `json:"index"`
	PreviousHash string            `json:"previousHash"`
	Timestamp    float64           `json:"timestamp"`
	Nonce        int64             `json:"nonce"`
	Transactions []json.RawMessage `json:"transactions"`
	Hash         string            `json:"hash"`
}

// candidateResp is the JSON returned by GET /miner/candidate.
type candidateResp struct {
	Block      block  `json:"block"`
	Difficulty uint64 `json:"difficulty"`
}

// ── hashing ──────────────────────────────────────────────────────────────────

// sha256hex returns the SHA-256 digest of s as a lowercase hex string.
// Used only for txRoot — block hashes use argon2BlockHash.
func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// argon2BlockHash computes the Argon2id hash of the block header data string.
// Parameters must match config/config.go exactly.
func argon2BlockHash(data string) string {
	h := argon2.IDKey(
		[]byte(data),
		[]byte(argon2Salt),
		argon2Iterations,
		argon2Memory,
		argon2Parallelism,
		argon2KeyLen,
	)
	return hex.EncodeToString(h)
}

// txRoot commits to the block's transaction set without embedding full
// ML-DSA44 signatures in the PoW hash. Each transaction hash already commits
// to that transaction's complete content.
// The hash field is extracted from each raw JSON message; all other fields
// are ignored for hashing but preserved intact for submission to the node.
func txRoot(txs []json.RawMessage) string {
	parts := make([]string, len(txs))
	for i, raw := range txs {
		var tx struct {
			Hash string `json:"hash"`
		}
		_ = json.Unmarshal(raw, &tx)
		parts[i] = tx.Hash
	}
	return sha256hex(strings.Join(parts, ""))
}

// formatTimestamp renders a float64 timestamp exactly as the node does,
// matching JavaScript's Number.toString(): whole numbers have no decimal point.
func formatTimestamp(ts float64) string {
	if ts == math.Trunc(ts) {
		return fmt.Sprintf("%d", int64(ts))
	}
	return strconv.FormatFloat(ts, 'f', -1, 64)
}

func (b *block) computeHash() string {
	data := fmt.Sprintf("%d%s%s%s%d",
		b.Index,
		b.PreviousHash,
		formatTimestamp(b.Timestamp),
		txRoot(b.Transactions),
		b.Nonce,
	)
	return argon2BlockHash(data)
}

// getDifficulty interprets the first 14 hex characters of the hash as a
// uint64. A lower value means more work was done (harder to achieve).
func getDifficulty(hash string) uint64 {
	if len(hash) < 14 {
		return math.MaxUint64
	}
	val, err := strconv.ParseUint(hash[:14], 16, 64)
	if err != nil {
		return math.MaxUint64
	}
	return val
}

// proveWork grinds the nonce across all available CPU cores until a hash
// whose difficulty is below the target is found. Returns the solved block.
func proveWork(b block, difficulty uint64) block {
	root := txRoot(b.Transactions)
	indexStr := strconv.Itoa(b.Index)
	workers := runtime.NumCPU()

	done := make(chan block, 1)
	quit := make(chan struct{})
	var closeOnce sync.Once

	for w := 0; w < workers; w++ {
		go func(startNonce int64) {
			local := b
			local.Nonce = startNonce
			for {
				select {
				case <-quit:
					return
				default:
				}
				local.Timestamp = float64(time.Now().UnixMilli()) / 1000.0
				data := indexStr + local.PreviousHash + formatTimestamp(local.Timestamp) + root + strconv.FormatInt(local.Nonce, 10)
				local.Hash = argon2BlockHash(data)
				if getDifficulty(local.Hash) < difficulty {
					select {
					case done <- local:
						closeOnce.Do(func() { close(quit) })
					default:
					}
					return
				}
				local.Nonce += int64(workers)
			}
		}(int64(w))
	}

	return <-done
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

func httpGet(url string, out interface{}) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func httpPost(url string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// isHTTPError reports whether err is an HTTP-level error (4xx/5xx response)
// as opposed to a network-level failure (connection refused, timeout, etc.).
func isHTTPError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "HTTP")
}

func truncHash(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:8] + "…" + h[len(h)-8:]
}

// ── peer list ─────────────────────────────────────────────────────────────────

// fetchPeers downloads a newline-delimited peers.txt and returns the URLs.
// Lines starting with # are treated as comments. Returns nil on any error.
func fetchPeers(peersURL string) []string {
	resp, err := httpClient.Get(peersURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var peers []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			peers = append(peers, line)
		}
	}
	return peers
}

// peerPool holds a shuffled peer list and cycles through it on failure.
type peerPool struct {
	all     []string
	ordered []string
	idx     int
}

func newPeerPool(peers []string) *peerPool {
	p := &peerPool{all: peers}
	p.reshuffle()
	return p
}

func (p *peerPool) reshuffle() {
	ordered := make([]string, len(p.all))
	copy(ordered, p.all)
	rand.Shuffle(len(ordered), func(i, j int) { ordered[i], ordered[j] = ordered[j], ordered[i] })
	p.ordered = ordered
	p.idx = 0
}

func (p *peerPool) current() string { return p.ordered[p.idx%len(p.ordered)] }

// markFailed advances to the next peer. Returns false when all have been tried.
func (p *peerPool) markFailed() bool {
	p.idx++
	return p.idx < len(p.ordered)
}

func (p *peerPool) reset() { p.reshuffle() }

// versionOK checks that the node reports the expected protocol version.
func versionOK(nodeURL string) (bool, error) {
	var resp struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := httpGet(nodeURL+"/node/version", &resp); err != nil {
		return false, err
	}
	return resp.ProtocolVersion == protocolVersion, nil
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	reward   := flag.String("reward", "", "your 64-character GRPD reward address (required)")
	nodeURL  := flag.String("node", defaultNode, "GRPD node URL (used if -peers is unavailable)")
	peersURL := flag.String("peers", defaultPeersURL, "URL of peers.txt peer list")
	flag.Parse()

	if *reward == "" || len(*reward) != 64 {
		fmt.Fprintln(os.Stderr, "error: -reward must be a 64-character GRPD address")
		fmt.Fprintln(os.Stderr, "usage: grpd-miner -reward <address> [-node <url>] [-peers <url>]")
		os.Exit(1)
	}

	// Build peer pool — prefer peers.txt, fall back to -node.
	peers := fetchPeers(*peersURL)
	if len(peers) == 0 {
		fmt.Printf("peers.txt unavailable — using %s\n", *nodeURL)
		peers = []string{*nodeURL}
	}
	pool := newPeerPool(peers)

	// Version-check the initial peer; skip to next if mismatched or unreachable.
	checked := false
	for i := 0; i < len(peers); i++ {
		ok, err := versionOK(pool.current())
		if err != nil {
			fmt.Printf("peer %s unreachable: %v\n", pool.current(), err)
			pool.markFailed()
			continue
		}
		if !ok {
			fmt.Printf("peer %s has wrong protocol version — skipping\n", pool.current())
			pool.markFailed()
			continue
		}
		checked = true
		break
	}
	if !checked {
		fmt.Fprintln(os.Stderr, "error: no reachable peer with matching protocol version")
		os.Exit(1)
	}

	fmt.Printf("GRPD Miner\n")
	fmt.Printf("  peers:    %d known (%s)\n", len(peers), *peersURL)
	fmt.Printf("  reward:   %s\n", *reward)
	fmt.Printf("  workers:  %d CPU cores\n\n", runtime.NumCPU())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	mined := 0
	errors := 0
	started := time.Now()

	for {
		select {
		case <-stop:
			elapsed := time.Since(started).Round(time.Second)
			fmt.Printf("\nstopped — %d block(s) mined, %d error(s), elapsed %s\n",
				mined, errors, elapsed)
			return
		default:
		}

		node := pool.current()
		var candidate candidateResp
		candidateURL := fmt.Sprintf("%s/miner/candidate?rewardAddress=%s", node, *reward)
		if err := httpGet(candidateURL, &candidate); err != nil {
			errors++
			fmt.Printf("[%s] %s — fetch error: %v\n", time.Now().Format("15:04:05"), node, err)
			if !pool.markFailed() {
				fmt.Printf("[%s] all peers failed — waiting 10s\n", time.Now().Format("15:04:05"))
				pool.reset()
				select {
				case <-stop:
					return
				case <-time.After(10 * time.Second):
				}
			}
			continue
		}

		fmt.Printf("[%s] mining block #%d  difficulty=%d_  peer=%s\n",
			time.Now().Format("15:04:05"), candidate.Block.Index, candidate.Difficulty, node)

		solved := proveWork(candidate.Block, candidate.Difficulty)

		if err := httpPost(node+"/miner/submit", solved); err != nil {
			errors++
			fmt.Printf("[%s] submit error: %v\n", time.Now().Format("15:04:05"), err)
			// A 400 rejection (stale block, lost race) is not a peer failure —
			// the peer is healthy, we just lost the race. Only mark the peer
			// failed on network-level errors (connection refused, timeout, etc.).
			if !isHTTPError(err) {
				pool.markFailed()
			}
			continue
		}

		mined++
		fmt.Printf("[%s] block #%d accepted  nonce=%d  hash=%s\n",
			time.Now().Format("15:04:05"), solved.Index, solved.Nonce, truncHash(solved.Hash))
	}
}
