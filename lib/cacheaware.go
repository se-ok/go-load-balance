package lib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sync"
	"time"
)

// Cache-aware routing: requests sharing a prefix (same conversation, same
// document, same harness first turn) are routed to the node that already
// holds the matching KV cache. The mechanism is chunked-turn chained hashing
// over a sticky table — no radix trie, no text retention; see CLAUDE.md.

const (
	// affinityBlockChars is both the oversized-turn threshold and the block
	// size: canonical turn text longer than this is split into blocks of this
	// many bytes, so long shared prefixes inside one turn (same document,
	// different question) still match at block granularity.
	affinityBlockChars = 8192
	// affinityMaxUnits freezes the chain: units beyond this are ignored, so
	// ultra-long conversations keep a stable deepest key.
	affinityMaxUnits = 500
	// affinityOverflowFraction: a warm pick overflows to least-connections
	// when its connection lead over the least-loaded node exceeds this
	// fraction of maxConns.
	affinityOverflowFraction = 0.2
	// affinityMaxBody bounds the in-memory copy of a request body made for
	// key computation — a DoS guard only. Real LLM requests are a few MB even
	// at 256k-token context; bodies over the cap get 413.
	affinityMaxBody = 1 << 30 // 1 GiB
	// affinityRetentionFactor: per backend, retain the hash entries of at
	// most maxConns * this many recent requests.
	affinityRetentionFactor = 5
)

// affinityEntry pins one chain hash to a backend. epoch must still match the
// backend's health epoch for the pin to be valid.
type affinityEntry struct {
	backend  int
	epoch    uint64
	lastSeen time.Time
}

// requestRecord remembers which hashes one request upserted, for per-backend
// retention eviction.
type requestRecord struct {
	hashes [][16]byte
	at     time.Time
}

type affinityState struct {
	ttl      time.Duration
	maxConns int

	mu    sync.Mutex
	now   func() time.Time // injectable for tests
	table map[[16]byte]affinityEntry
	// per-backend FIFO of recent requests, capped at maxConns *
	// affinityRetentionFactor records
	records map[int][]requestRecord

	// counters since the last status line
	warm, cold, overflow uint64
}

// EnableCacheAware switches the pool to cache-aware routing. ttl is the
// sliding lifetime of affinity entries; maxConns (> 0) is the hard
// per-backend connection cap the load guard and retention are scaled by.
// Call before serving traffic.
func (p *Pool) EnableCacheAware(ttl time.Duration, maxConns int) {
	p.maxConns = maxConns
	p.affinity = &affinityState{
		ttl:      ttl,
		maxConns: maxConns,
		now:      time.Now,
		table:    make(map[[16]byte]affinityEntry),
		records:  make(map[int][]requestRecord),
	}
}

// canonicalMessage builds the canonical text of one chat message from its
// content-bearing fields in fixed order: role, reasoning (falling back to
// reasoning_content), content, tool_calls, tool_call_id. A turn with empty
// content but tool_calls present therefore still hashes distinctly. Raw JSON
// bytes are used as-is: identical requests are byte-identical, which is all
// hashing needs.
func canonicalMessage(raw json.RawMessage) []byte {
	var msg struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		Reasoning        json.RawMessage `json:"reasoning"`
		ReasoningContent json.RawMessage `json:"reasoning_content"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return raw
	}
	reasoning := msg.Reasoning
	if reasoning == nil {
		reasoning = msg.ReasoningContent
	}
	var buf bytes.Buffer
	buf.WriteString(msg.Role)
	buf.WriteByte(0x1f)
	buf.Write(reasoning)
	buf.WriteByte(0x1f)
	buf.Write(msg.Content)
	buf.WriteByte(0x1f)
	buf.Write(msg.ToolCalls)
	buf.WriteByte(0x1f)
	buf.WriteString(msg.ToolCallID)
	return buf.Bytes()
}

// splitUnits appends text to units, splitting oversized turns into
// affinityBlockChars-sized blocks.
func splitUnits(units [][]byte, text []byte) [][]byte {
	for len(text) > affinityBlockChars {
		units = append(units, text[:affinityBlockChars])
		text = text[affinityBlockChars:]
	}
	return append(units, text)
}

// chainUnits computes the cumulative hash chain c_i = H(c_{i-1} || unit_i).
// Equal c_i proves the first i units are byte-identical, so each hash
// identifies an entire prefix, and the table can stay a flat map.
func chainUnits(units [][]byte) [][16]byte {
	if len(units) == 0 {
		return nil
	}
	if len(units) > affinityMaxUnits {
		units = units[:affinityMaxUnits]
	}
	chain := make([][16]byte, len(units))
	for i, u := range units {
		h := fnv.New128a()
		if i > 0 {
			h.Write(chain[i-1][:])
		}
		h.Write(u)
		copy(chain[i][:], h.Sum(nil))
	}
	return chain
}

// affinityChain derives the request's prefix hash chain. Priority: the
// standard OpenAI `user` field when set (an explicit session identity from
// the caller), else the chat message stream, else the raw prompt. Returns nil
// when no key can be derived — the caller falls back to least-connections.
func affinityChain(body []byte) [][16]byte {
	if len(body) == 0 {
		return nil
	}
	var req struct {
		User     string            `json:"user"`
		Messages []json.RawMessage `json:"messages"`
		Prompt   json.RawMessage   `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	if req.User != "" {
		return chainUnits(splitUnits(nil, []byte(req.User)))
	}
	var units [][]byte
	if len(req.Messages) > 0 {
		for _, m := range req.Messages {
			units = splitUnits(units, canonicalMessage(m))
			if len(units) >= affinityMaxUnits {
				break
			}
		}
	} else if len(req.Prompt) > 0 && !bytes.Equal(req.Prompt, []byte("null")) {
		units = splitUnits(units, req.Prompt)
	}
	return chainUnits(units)
}

// selectCacheAware picks a backend for the given chain and reserves a
// connection slot on it. Runs under the pool lock like SelectBackend, keeping
// the ±1 burst guarantee. nil chain means "no derivable key" and places by
// least-connections without touching the table.
func (p *Pool) selectCacheAware(chain [][16]byte) (*Backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	a := p.affinity
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	least, leastIdx, leastErr := p.leastConnLocked()

	// Walk the chain deepest-first for the longest still-valid pin.
	pinnedIdx := -1
	for i := len(chain) - 1; i >= 0; i-- {
		e, ok := a.table[chain[i]]
		if !ok {
			continue
		}
		b := p.backends[e.backend]
		if now.Sub(e.lastSeen) > a.ttl || e.epoch != b.Epoch() {
			delete(a.table, chain[i]) // expired or backend went down since
			continue
		}
		if !b.IsHealthy() {
			continue
		}
		pinnedIdx = e.backend
		break
	}

	var winner *Backend
	winnerIdx := -1
	if pinnedIdx >= 0 {
		pinned := p.backends[pinnedIdx]
		pc := pinned.GetActiveConns()
		// Load guard: overflow to least-connections when the pinned node is
		// at the hard cap, or its lead over the least-loaded node exceeds
		// affinityOverflowFraction of the cap.
		over := pc >= a.maxConns
		if !over && leastErr == nil {
			gap := pc - least.GetActiveConns()
			over = float64(gap) > affinityOverflowFraction*float64(a.maxConns)
		}
		if over {
			if leastErr != nil {
				return nil, leastErr
			}
			winner, winnerIdx = least, leastIdx
			a.overflow++
		} else {
			winner, winnerIdx = pinned, pinnedIdx
			a.warm++
		}
	} else {
		if leastErr != nil {
			return nil, leastErr
		}
		winner, winnerIdx = least, leastIdx
		a.cold++
	}

	winner.IncrementConns()

	if len(chain) > 0 {
		a.upsertLocked(chain, winnerIdx, winner.Epoch(), now)
	}
	return winner, nil
}

// upsertLocked re-points every chain hash at the backend that actually
// serves the request (pin-follows-reality) and applies per-backend retention.
// Caller must hold a.mu.
func (a *affinityState) upsertLocked(chain [][16]byte, idx int, epoch uint64, now time.Time) {
	for _, h := range chain {
		a.table[h] = affinityEntry{backend: idx, epoch: epoch, lastSeen: now}
	}
	a.records[idx] = append(a.records[idx], requestRecord{hashes: chain, at: now})

	limit := a.maxConns * affinityRetentionFactor
	for len(a.records[idx]) > limit {
		old := a.records[idx][0]
		a.records[idx] = a.records[idx][1:]
		for _, h := range old.hashes {
			// Only drop hashes this backend still owns and that no newer
			// request has refreshed since.
			if e, ok := a.table[h]; ok && e.backend == idx && !e.lastSeen.After(old.at) {
				delete(a.table, h)
			}
		}
	}
}

// serveCacheAware buffers the request body (bounded), derives the affinity
// chain, selects a backend, and proxies. The buffered body also enables
// r.GetBody, letting the transport transparently retry a request that failed
// on a reused connection.
func (p *Pool) serveCacheAware(w http.ResponseWriter, r *http.Request) {
	var chain [][16]byte
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, affinityMaxBody)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
			}
			return
		}
		chain = affinityChain(raw)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		}
		r.ContentLength = int64(len(raw))
	}

	backend, err := p.selectCacheAware(chain)
	if err != nil {
		writeSelectError(w, err)
		return
	}
	defer backend.DecrementConns()

	backend.GetProxy().ServeHTTP(w, r)
}

// affinityStatsLine reports and resets the routing counters since the last
// status line.
func (p *Pool) affinityStatsLine() string {
	a := p.affinity
	a.mu.Lock()
	warm, cold, ovfl := a.warm, a.cold, a.overflow
	a.warm, a.cold, a.overflow = 0, 0, 0
	keys := len(a.table)
	a.mu.Unlock()

	total := warm + cold + ovfl
	if total == 0 {
		return fmt.Sprintf("Affinity: idle (%d keys)", keys)
	}
	pct := func(n uint64) uint64 { return n * 100 / total }
	return fmt.Sprintf("Affinity: warm %d%% cold %d%% ovfl %d%% (%d reqs, %d keys)",
		pct(warm), pct(cold), pct(ovfl), total, keys)
}
