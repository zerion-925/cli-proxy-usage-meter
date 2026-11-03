package meter

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Native Claude callbacks preserve thinking as a subset of OutputTokens. The
// host's structured breakdown carries that fact, but the native plugin ABI does not.
func claudeUsage(at time.Time) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID: "claude-execution", APIKey: "synthetic-client", IsNativeKey: true,
		AccessProvider: "config-inline", Provider: "claude", ExecutorType: "ClaudeExecutor",
		Model: "model", RequestedAt: at,
		Detail: pluginapi.UsageDetail{
			InputTokens: 10, OutputTokens: 200, ReasoningTokens: 50,
			CacheReadTokens: 500, CachedTokens: 500, CacheCreationTokens: 20,
			TotalTokens: 730,
		},
	}
}

func TestClaudeThinkingIsAlreadyIncludedInOutput(t *testing.T) {
	r := claudeUsage(time.Now())
	for _, executor := range []string{"ClaudeExecutor", ""} {
		t.Run("native/"+executor, func(t *testing.T) {
			r.ExecutorType = executor
			b := tokenBreakdown(r)
			m := tokenMetrics(r)
			if b.Quality != usage.TokenAccountingQualityComplete || b.TotalTokens != 730 ||
				b.Input.TotalTokens != 530 || b.Input.UncachedTokens != 10 ||
				b.Output.TotalTokens != 200 || b.Output.NonReasoningTokens != 150 || b.Output.ReasoningTokens != 50 ||
				m.TotalTokens != 730 || m.OutputTokens != 200 || m.UnknownTokenEvents != 0 || m.CacheRateDenominator != 530 {
				t.Fatalf("thinking was double-counted: breakdown=%+v metrics=%+v", b, m)
			}
		})
	}

	t.Run("compatible executor retains inclusive input semantics", func(t *testing.T) {
		r := claudeUsage(time.Now())
		r.ExecutorType = "OpenAICompatExecutor"
		r.Detail.InputTokens = 530
		b := tokenBreakdown(r)
		if b.Quality != usage.TokenAccountingQualityComplete || b.Input.UncachedTokens != 10 || b.TotalTokens != 730 {
			t.Fatalf("native Claude rule changed compatible accounting: %+v", b)
		}
	})
	for _, invalid := range []string{"thinking exceeds output", "inconsistent total"} {
		t.Run(invalid, func(t *testing.T) {
			r := claudeUsage(time.Now())
			if invalid == "thinking exceeds output" {
				r.Detail.ReasoningTokens = 201
			} else {
				r.Detail.TotalTokens++
			}
			if m := tokenMetrics(r); m.UnknownTokenEvents != 1 {
				t.Fatalf("invalid accounting accepted: %+v", m)
			}
		})
	}
}

func TestClaudeStandardCacheWritePricingAndThinking(t *testing.T) {
	c, err := parseCatalog([]byte(`{"anthropic":{"models":{"model":{"cost":{"input":2,"output":8,"cache_read":0.5,"cache_write":3}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := claudeUsage(time.Now())
	m := tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	// Independent input/cache buckets; all 200 output tokens include the 50 thinking tokens.
	wantCost := float64(10*2+500*0.5+20*3+200*8) / 1e6
	wantSavings := float64(500) * (2 - 0.5) / 1e6
	if m.UnknownTokenEvents != 0 || m.PricedAttempts != 1 || m.EstimatedCostUSD == nil ||
		math.Abs(*m.EstimatedCostUSD-wantCost) > 1e-12 || m.SavingsPricedAttempts != 1 ||
		m.EstimatedCacheSavingsUSD == nil || math.Abs(*m.EstimatedCacheSavingsUSD-wantSavings) > 1e-12 {
		t.Fatalf("standard write estimate: %+v, want cost %g and savings %g", m, wantCost, wantSavings)
	}

	// Missing cache-write prices remain unknown; a real published zero is free.
	model := c.Models["anthropic"]["model"]
	model.Rates.CacheWrite = nil
	c.Models["anthropic"]["model"] = model
	m = tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	if m.PricedAttempts != 0 || m.EstimatedCostUSD != nil {
		t.Fatalf("fabricated a missing cache-write rate: %+v", m)
	}
	model.Rates.CacheWrite = ptr(0)
	c.Models["anthropic"]["model"] = model
	m = tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	if m.PricedAttempts != 1 || m.EstimatedCostUSD == nil || math.Abs(*m.EstimatedCostUSD-(wantCost-0.00006)) > 1e-12 {
		t.Fatalf("explicit free cache writes: %+v", m)
	}
}

func TestClaudePricingPersistsAndReconcilesWithoutRepricingHistory(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(filepath.Join(t.TempDir(), "data"))
	cfg.PricingEnabled = true
	s, err := openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := claudeUsage(now.Add(-time.Minute))
	// This row predates the catalog and must remain unpriced after pricing is fixed.
	ingest(t, s, r, now)
	if err := s.refreshPricing(context.Background(), func(context.Context) ([]byte, error) {
		return []byte(`{"anthropic":{"models":{"model":{"cost":{"input":2,"output":8,"cache_read":0.5,"cache_write":3}}}}}`), nil
	}, now); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "priced-a"
	ingest(t, s, r, now)
	r.RequestID = "priced-b"
	r.APIKey = "second-synthetic-client"
	ingest(t, s, r, now)
	a := query(t, s, Query{}, now)
	if a.Pricing.PricedAttempts != 2 || a.Pricing.UnpricedAttempts != 1 || a.Totals.CacheRate == nil ||
		a.Totals.UnknownTokenEvents != 0 || a.Totals.TotalTokens != 2190 || a.Totals.EstimatedCostUSD == nil {
		t.Fatalf("Claude usage did not persist: %+v", a)
	}
	var cost, tokens float64
	for _, client := range a.Clients {
		part := query(t, s, Query{Client: client.ID}, now)
		if part.Totals.EstimatedCostUSD != nil {
			cost += *part.Totals.EstimatedCostUSD
		}
		tokens += float64(part.Totals.TotalTokens)
	}
	if math.Abs(cost-*a.Totals.EstimatedCostUSD) > 1e-12 || tokens != float64(a.Totals.TotalTokens) ||
		len(a.Models) != 1 || !reflect.DeepEqual(a.Models[0].Metrics, a.Totals) {
		t.Fatalf("client/model totals do not reconcile: %+v", a)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := query(t, s, Query{}, now)
	if !reflect.DeepEqual(a.Totals, b.Totals) || !reflect.DeepEqual(a.Pricing, b.Pricing) || !reflect.DeepEqual(a.Clients, b.Clients) {
		t.Fatalf("restart changed saved pricing: before=%+v after=%+v", a, b)
	}
}
