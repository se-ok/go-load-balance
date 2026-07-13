package lib

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- helpers ---

func chatBody(t *testing.T, messages ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"messages": messages, "max_tokens": 10})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

// newCacheAwarePool builds a pool of n backends with cache-aware routing
// enabled and a controllable clock. Backends are never dialed in unit tests.
func newCacheAwarePool(t *testing.T, n, maxConns int, ttl time.Duration) (*Pool, *time.Time) {
	t.Helper()
	urls := make([]string, n)
	for i := range n {
		urls[i] = fmt.Sprintf("http://backend-%d", i)
	}
	pool, err := NewPool(urls)
	if err != nil {
		t.Fatal(err)
	}
	pool.EnableCacheAware(ttl, maxConns)
	clock := time.Unix(1_000_000, 0)
	pool.affinity.now = func() time.Time { return clock }
	return pool, &clock
}

// selectAndRelease routes one request and immediately releases its slot, so
// tests can steer placement purely via manually-set activeConns.
func selectAndRelease(t *testing.T, p *Pool, body []byte) *Backend {
	t.Helper()
	b, err := p.selectCacheAware(affinityChain(body))
	if err != nil {
		t.Fatalf("selectCacheAware: %v", err)
	}
	b.DecrementConns()
	return b
}

func counters(p *Pool) (warm, cold, overflow uint64) {
	a := p.affinity
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.warm, a.cold, a.overflow
}

// --- chain derivation ---

func TestAffinityChainTurnUnits(t *testing.T) {
	short := chatBody(t, msg("system", "S"), msg("user", "u1"))
	if got := len(affinityChain(short)); got != 2 {
		t.Errorf("expected 2 units for 2 small turns, got %d", got)
	}

	// Prefix property: the chain of an extended conversation starts with the
	// chain of its prefix.
	long := chatBody(t, msg("system", "S"), msg("user", "u1"), msg("assistant", "a1"))
	c1, c2 := affinityChain(short), affinityChain(long)
	if len(c2) != 3 {
		t.Fatalf("expected 3 units, got %d", len(c2))
	}
	for i := range c1 {
		if c1[i] != c2[i] {
			t.Errorf("prefix property violated at unit %d", i)
		}
	}

	// Determinism
	again := affinityChain(chatBody(t, msg("system", "S"), msg("user", "u1")))
	for i := range c1 {
		if c1[i] != again[i] {
			t.Errorf("chain not deterministic at unit %d", i)
		}
	}
}

func TestAffinityChainOversizedTurnSplits(t *testing.T) {
	// ~20k chars of content -> canonical text > 2 blocks -> 3 units,
	// and a same-document-different-question body shares the leading blocks.
	doc := strings.Repeat("d", 20_000)
	a := chatBody(t, msg("user", doc+" question-one"))
	b := chatBody(t, msg("user", doc+" question-two"))
	ca, cb := affinityChain(a), affinityChain(b)
	if len(ca) != 3 {
		t.Fatalf("expected 3 block units for 20k-char turn, got %d", len(ca))
	}
	if ca[0] != cb[0] || ca[1] != cb[1] {
		t.Error("shared document blocks should hash equal")
	}
	if ca[2] == cb[2] {
		t.Error("divergent final block should hash differently")
	}
}

func TestAffinityChainUnitCapFreezes(t *testing.T) {
	msgs := make([]map[string]any, 0, affinityMaxUnits+100)
	for i := range affinityMaxUnits + 100 {
		msgs = append(msgs, msg("user", fmt.Sprintf("turn %d", i)))
	}
	if got := len(affinityChain(chatBody(t, msgs...))); got != affinityMaxUnits {
		t.Errorf("expected chain frozen at %d units, got %d", affinityMaxUnits, got)
	}
}

func TestAffinityChainUserOverride(t *testing.T) {
	a := []byte(`{"user":"session-1","messages":[{"role":"user","content":"x"}]}`)
	b := []byte(`{"user":"session-1","messages":[{"role":"user","content":"totally different"}]}`)
	ca, cb := affinityChain(a), affinityChain(b)
	if len(ca) != 1 || len(cb) != 1 || ca[0] != cb[0] {
		t.Error("user field should override content-derived keys")
	}
}

func TestAffinityChainPrompt(t *testing.T) {
	if got := len(affinityChain([]byte(`{"prompt":"hello world"}`))); got != 1 {
		t.Errorf("string prompt: expected 1 unit, got %d", got)
	}
	if got := affinityChain([]byte(`{"prompt":["a","b"]}`)); got == nil {
		t.Error("array prompt should derive a chain")
	}
	if got := affinityChain([]byte(`not json`)); got != nil {
		t.Error("invalid JSON should derive no chain")
	}
	if got := affinityChain(nil); got != nil {
		t.Error("empty body should derive no chain")
	}
}

func TestAffinityChainContentFields(t *testing.T) {
	// tool_calls-only turn (empty content) must still hash distinctly
	tc1 := chatBody(t, msg("user", "u"), map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []map[string]any{{"id": "call_1", "function": map[string]any{"name": "f", "arguments": "{}"}}},
	})
	tc2 := chatBody(t, msg("user", "u"), map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []map[string]any{{"id": "call_2", "function": map[string]any{"name": "g", "arguments": "{}"}}},
	})
	a, b := affinityChain(tc1), affinityChain(tc2)
	if a[1] == b[1] {
		t.Error("different tool_calls with empty content should hash differently")
	}

	// reasoning and reasoning_content fill the same canonical slot
	r1 := chatBody(t, map[string]any{"role": "assistant", "content": "c", "reasoning": "think"})
	r2 := chatBody(t, map[string]any{"role": "assistant", "content": "c", "reasoning_content": "think"})
	r3 := chatBody(t, map[string]any{"role": "assistant", "content": "c", "reasoning": "other"})
	if affinityChain(r1)[0] != affinityChain(r2)[0] {
		t.Error("reasoning and reasoning_content with same value should hash equal")
	}
	if affinityChain(r1)[0] == affinityChain(r3)[0] {
		t.Error("different reasoning should hash differently")
	}
}

// --- selection behavior ---

func TestCacheAwareColdPlacesLeastConn(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 3, 10, time.Hour)
	pool.backends[0].activeConns = 5
	pool.backends[1].activeConns = 0
	pool.backends[2].activeConns = 3

	b := selectAndRelease(t, pool, chatBody(t, msg("user", "fresh")))
	if b != pool.backends[1] {
		t.Errorf("cold key should place on least-loaded backend, got %s", b.URL)
	}
	if _, cold, _ := counters(pool); cold != 1 {
		t.Errorf("expected 1 cold, got %d", cold)
	}
}

func TestCacheAwareWarmSticksAndExtends(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 3, 10, time.Hour)
	conv := chatBody(t, msg("system", "S"), msg("user", "u1"))
	home := selectAndRelease(t, pool, conv)

	// Same conversation extended by later turns still routes home even when
	// other backends are idle and home is (mildly) busier.
	home.activeConns = 1
	longer := chatBody(t, msg("system", "S"), msg("user", "u1"), msg("assistant", "a1"), msg("user", "u2"))
	for range 3 {
		if b := selectAndRelease(t, pool, longer); b != home {
			t.Fatalf("extended conversation should stay on %s, got %s", home.URL, b.URL)
		}
	}
	if warm, _, _ := counters(pool); warm != 3 {
		t.Errorf("expected 3 warm, got %d", warm)
	}
}

func TestCacheAwareGuardMaxConns(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 2, 10, time.Hour)
	conv := chatBody(t, msg("user", "pin me"))
	home := selectAndRelease(t, pool, conv)

	home.activeConns = 10 // at the hard cap
	b := selectAndRelease(t, pool, conv)
	if b == home {
		t.Error("pinned backend at max-conns must overflow")
	}
	if _, _, overflow := counters(pool); overflow != 1 {
		t.Errorf("expected 1 overflow, got %d", overflow)
	}

	// Pin follows reality: with home relieved, the key now lives on b.
	home.activeConns = 0
	if again := selectAndRelease(t, pool, conv); again != b {
		t.Error("pin should have re-pointed to the overflow target")
	}
}

func TestCacheAwareGuardGapFraction(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 2, 10, time.Hour) // 0.2 * 10 = gap threshold 2
	conv := chatBody(t, msg("user", "gap test"))
	home := selectAndRelease(t, pool, conv)
	other := pool.backends[0]
	if home == other {
		other = pool.backends[1]
	}

	// gap == 2: not over the threshold -> warm
	home.activeConns, other.activeConns = 2, 0
	if b := selectAndRelease(t, pool, conv); b != home {
		t.Error("gap equal to 0.2*maxConns should NOT overflow")
	}
	// gap == 3: over -> overflow
	home.activeConns = 3
	if b := selectAndRelease(t, pool, conv); b == home {
		t.Error("gap above 0.2*maxConns should overflow")
	}
}

func TestCacheAwareAllAtCapacity(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 2, 1, time.Hour)
	pool.backends[0].activeConns = 1
	pool.backends[1].activeConns = 1
	_, err := pool.selectCacheAware(affinityChain(chatBody(t, msg("user", "x"))))
	if !errors.Is(err, errAtCapacity) {
		t.Errorf("expected errAtCapacity when all healthy backends are full, got %v", err)
	}

	// Distinct from a real outage: no healthy backends at all.
	pool.backends[0].healthy = false
	pool.backends[1].healthy = false
	_, err = pool.selectCacheAware(affinityChain(chatBody(t, msg("user", "x"))))
	if !errors.Is(err, errNoHealthyBackends) {
		t.Errorf("expected errNoHealthyBackends, got %v", err)
	}
}

func TestCacheAwareTTLExpiry(t *testing.T) {
	pool, clock := newCacheAwarePool(t, 2, 10, time.Minute)
	conv := chatBody(t, msg("user", "expiring"))
	home := selectAndRelease(t, pool, conv)
	other := pool.backends[0]
	if home == other {
		other = pool.backends[1]
	}

	// Within TTL: still warm despite other being idle.
	*clock = clock.Add(50 * time.Second)
	home.activeConns = 1
	if b := selectAndRelease(t, pool, conv); b != home {
		t.Error("entry within sliding TTL should stay pinned")
	}

	// The hit above refreshed lastSeen (sliding): +50s more is still warm.
	*clock = clock.Add(50 * time.Second)
	if b := selectAndRelease(t, pool, conv); b != home {
		t.Error("sliding TTL should refresh on hit")
	}

	// Past TTL with no hits: entry expires, placement is by load again.
	*clock = clock.Add(2 * time.Minute)
	if b := selectAndRelease(t, pool, conv); b != other {
		t.Error("expired entry should re-place by least-connections")
	}
	if _, cold, _ := counters(pool); cold != 2 { // initial + post-expiry
		t.Errorf("expected 2 cold placements, got %d", cold)
	}
}

func TestCacheAwareEpochPurgeOnUnhealthy(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 2, 10, time.Hour)
	conv := chatBody(t, msg("user", "epoch"))
	home := selectAndRelease(t, pool, conv)

	// Backend goes down and recovers: same URL, new epoch -> old pin invalid.
	home.MarkUnhealthy()
	home.RecordCheckSuccess()
	home.RecordCheckSuccess() // healthyThreshold consecutive passes

	if !home.IsHealthy() {
		t.Fatal("backend should have recovered")
	}
	selectAndRelease(t, pool, conv)
	if _, cold, _ := counters(pool); cold != 2 {
		t.Errorf("recovered backend must not inherit pre-crash pins: expected 2 cold, got %d", cold)
	}
}

func TestCacheAwareUnhealthyPinRePlaces(t *testing.T) {
	pool, _ := newCacheAwarePool(t, 2, 10, time.Hour)
	conv := chatBody(t, msg("user", "failover"))
	home := selectAndRelease(t, pool, conv)
	other := pool.backends[0]
	if home == other {
		other = pool.backends[1]
	}

	home.MarkUnhealthy()
	if b := selectAndRelease(t, pool, conv); b != other {
		t.Errorf("pin to unhealthy backend should re-place on healthy one, got %s", b.URL)
	}
}

// --- retention ---

func TestCacheAwareRetentionEvictsOldRequests(t *testing.T) {
	// maxConns=1 -> retention = 5 records per backend; single backend so all
	// requests land on it deterministically.
	pool, _ := newCacheAwarePool(t, 1, 1, time.Hour)

	firstConv := chatBody(t, msg("user", "conv-0"))
	selectAndRelease(t, pool, firstConv)
	for i := 1; i <= 5; i++ {
		selectAndRelease(t, pool, chatBody(t, msg("user", fmt.Sprintf("conv-%d", i))))
	}

	// 6 requests with capacity 5: conv-0's entries must be gone.
	warmBefore, _, _ := counters(pool)
	selectAndRelease(t, pool, firstConv)
	warmAfter, cold, _ := counters(pool)
	if warmAfter != warmBefore {
		t.Error("evicted conversation should not be warm")
	}
	if cold != 7 { // 6 initial colds + this re-placement
		t.Errorf("expected 7 cold placements, got %d", cold)
	}
}

func TestCacheAwareRetentionKeepsReownedHashes(t *testing.T) {
	pool, clock := newCacheAwarePool(t, 1, 1, time.Hour)

	keeper := chatBody(t, msg("user", "keeper"))
	selectAndRelease(t, pool, keeper) // record 1
	for i := range 3 {
		selectAndRelease(t, pool, chatBody(t, msg("user", fmt.Sprintf("filler-%d", i))))
	}
	*clock = clock.Add(time.Second)
	selectAndRelease(t, pool, keeper) // re-owned with newer lastSeen (record 5)
	for i := range 3 {
		selectAndRelease(t, pool, chatBody(t, msg("user", fmt.Sprintf("late-%d", i))))
	}
	// keeper's first record has been evicted by now, but the refreshed entry
	// must have survived its eviction sweep.
	warmBefore, _, _ := counters(pool)
	selectAndRelease(t, pool, keeper)
	warmAfter, _, _ := counters(pool)
	if warmAfter != warmBefore+1 {
		t.Error("hash re-owned by a newer request must survive old record eviction")
	}
}
