package meter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// Small synthetic rates exercise arithmetic; these are never shipped as prices.
const testCatalog = `{"openai":{"models":{"model":{"cost":{"input":2,"output":8,"cache_read":0.5}}}},"reseller":{"models":{"model":{"cost":{"input":99,"output":99,"cache_read":99}}}}}`

func TestPricingDisjointBucketsAndProviderIsolation(t *testing.T) {
	c, err := parseCatalog([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	r := event("priced", "client", time.Now())
	m := tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	// 60 uncached + 40 cached + 20 output, all per million.
	if m.PricedAttempts != 1 || m.EstimatedCostUSD == nil || math.Abs(*m.EstimatedCostUSD-0.0003) > 1e-12 {
		t.Fatalf("disjoint cost: %+v", m)
	}
	if m.EstimatedCacheSavingsUSD == nil || math.Abs(*m.EstimatedCacheSavingsUSD-0.00006) > 1e-12 {
		t.Fatalf("savings: %+v", m)
	}
	r.Provider = "openai-compatibility"
	m = tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	if m.PricedAttempts != 0 || m.EstimatedCostUSD != nil {
		t.Fatal("guessed custom provider")
	}
}

func TestPricingConservativeCoverage(t *testing.T) {
	cases := []struct {
		name, provider, cost string
		detail               pluginapi.UsageDetail
		want                 *float64
	}{
		{"explicit zero", "openai", `{"input":0,"output":0,"cache_read":0}`, pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40}, ptr(0)},
		{"missing cached rate", "openai", `{"input":2,"output":8}`, pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40}, nil},
		{"positive only requirements", "openai", `{"output":8}`, pluginapi.UsageDetail{OutputTokens: 20}, ptr(.00016)},
		{"claude independent", "claude", `{"input":2,"output":8,"cache_read":0.5}`, pluginapi.UsageDetail{InputTokens: 60, CacheReadTokens: 40, OutputTokens: 15, ReasoningTokens: 5}, ptr(.0003)},
		{"openai inclusive reasoning", "openai", `{"input":2,"output":8,"reasoning":10}`, pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5}, ptr(.00037)},
		{"gemini separate reasoning", "gemini", `{"input":2,"output":8,"reasoning":10}`, pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5}, ptr(.00041)},
		{"tier boundary base", "openai", `{"input":2,"tiers":[{"tier":{"type":"context","size":100},"input":4}]}`, pluginapi.UsageDetail{InputTokens: 100}, ptr(.0002)},
		{"tier above", "openai", `{"input":2,"tiers":[{"tier":{"type":"context","size":100},"input":4}]}`, pluginapi.UsageDetail{InputTokens: 101}, ptr(.000404)},
		{"legacy tier", "openai", `{"input":2,"context_over_200k":{"input":4}}`, pluginapi.UsageDetail{InputTokens: 200001}, ptr(.800004)},
		{"unsupported tier", "openai", `{"input":2,"tiers":[{"tier":{"type":"service","size":100},"input":4}]}`, pluginapi.UsageDetail{InputTokens: 10}, nil},
		{"unknown price dimension", "openai", `{"input":2,"input_audio":10}`, pluginapi.UsageDetail{InputTokens: 10}, nil},
		{"cache TTL absent", "claude", `{"input":2,"cache_write":3}`, pluginapi.UsageDetail{InputTokens: 10, CacheCreationTokens: 10}, nil},
		{"zero counters", "openai", `{"input":0,"output":0}`, pluginapi.UsageDetail{}, nil},
		{"inconsistent", "openai", `{"input":2}`, pluginapi.UsageDetail{InputTokens: 10, TotalTokens: 11}, nil},
		{"unclassified", "openai", `{"input":2}`, pluginapi.UsageDetail{InputTokens: 10, CachedTokens: 5}, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			provider := map[string]string{"openai": "openai", "claude": "anthropic", "gemini": "google"}[tt.provider]
			c, err := parseCatalog([]byte(fmt.Sprintf(`{%q:{"models":{"model":{"cost":%s}}}}`, provider, tt.cost)))
			if err != nil {
				t.Fatal(err)
			}
			r := event("test", "key", time.Now())
			r.Provider = tt.provider
			r.Detail = tt.detail
			m := tokenMetrics(r)
			c.estimate(DefaultConfig("/unused"), r, &m)
			if tt.want == nil {
				if m.EstimatedCostUSD != nil {
					t.Fatalf("unexpected cost %v", *m.EstimatedCostUSD)
				}
			} else if m.EstimatedCostUSD == nil || math.Abs(*m.EstimatedCostUSD-*tt.want) > 1e-12 {
				t.Fatalf("cost %+v want %g", m, *tt.want)
			}
		})
	}
}
func ptr(v float64) *float64 { return &v }

func TestEventPricePersistenceAndCoverage(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "data"))
	s, err := openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := event("old", "client-a", now.Add(-time.Minute))
	ingest(t, s, r, now)
	fetch := func(context.Context) ([]byte, error) { return []byte(testCatalog), nil }
	if err := s.refreshPricing(context.Background(), fetch, now); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "priced"
	ingest(t, s, r, now)
	ingest(t, s, r, now)
	a := query(t, s, Query{}, now)
	if !a.Pricing.Available || a.Pricing.PricedAttempts != 1 || a.Pricing.UnpricedAttempts != 1 || a.Totals.EstimatedCostUSD == nil || len(a.Pricing.SnapshotVersions) != 1 {
		t.Fatalf("coverage %+v", a)
	}
	if a.Pricing.Status != "ready" || a.Pricing.SnapshotVersion == nil {
		t.Fatalf("catalog %+v", a.Pricing)
	}
	if err := s.refreshPricing(context.Background(), func(context.Context) ([]byte, error) {
		return []byte(strings.ReplaceAll(testCatalog, `"input":2`, `"input":4`)), nil
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "new-price"
	r.APIKey = "client-b"
	ingest(t, s, r, now)
	b := query(t, s, Query{}, now)
	if len(b.Pricing.SnapshotVersions) != 2 || math.Abs(*b.Totals.EstimatedCostUSD-.00072) > 1e-12 {
		t.Fatalf("immutable estimate %+v", b.Pricing)
	}
	var sum float64
	for _, client := range b.Clients {
		part := query(t, s, Query{Client: client.ID}, now)
		if part.Totals.EstimatedCostUSD != nil {
			sum += *part.Totals.EstimatedCostUSD
		}
	}
	if math.Abs(sum-*b.Totals.EstimatedCostUSD) > 1e-12 {
		t.Fatal("client reconciliation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := query(t, s, Query{}, now)
	if !reflect.DeepEqual(b.Pricing, c.Pricing) || b.Clients[0] != c.Clients[0] {
		t.Fatalf("restart changed estimates: %+v %+v", b.Pricing, c.Pricing)
	}
	if err := s.refreshPricing(context.Background(), func(context.Context) ([]byte, error) { return nil, errors.New("private-error") }, now); err == nil {
		t.Fatal("expected refresh error")
	}
	d := query(t, s, Query{}, now)
	if d.Pricing.Status != "stale" || *d.Pricing.SnapshotVersion != *c.Pricing.SnapshotVersion || *d.Pricing.EstimatedCostUSD != *c.Pricing.EstimatedCostUSD || strings.Contains(d.Pricing.Reason, "private-error") {
		t.Fatalf("cached fallback %+v", d.Pricing)
	}
}

func TestCatalogValidationAndExplicitMapping(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, testCatalog + `{}`, `{"openai":{"models":{"model":{"cost":{"input":-1}}}}}`,
		`{"openai":{"models":{"model":{"cost":{"input":1e999}}}}}`,
		`{"openai":{"models":{"model":{"cost":{"input":2,"input":3}}}}}`,
	} {
		if _, err := parseCatalog([]byte(raw)); err == nil {
			t.Errorf("accepted invalid catalog %s", raw)
		}
	}
	c, err := parseCatalog([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig("/unused")
	if err := yaml.Unmarshal([]byte("pricing_provider_map: '{\"custom\":\"reseller\"}'\npricing_model_map:\n  custom/alias: model\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := event("alias", "key", time.Now())
	r.Provider = "custom"
	r.Model = "alias"
	r.ExecutorType = "OpenAICompatExecutor"
	m := tokenMetrics(r)
	c.estimate(cfg, r, &m)
	if m.EstimatedCostUSD == nil || math.Abs(*m.EstimatedCostUSD-.01188) > 1e-12 {
		t.Fatalf("explicit mapping: %+v", m)
	}
	for _, tier := range []string{"priority", "flex", "auto", "secret-tier"} {
		r.ServiceTier = tier
		m = tokenMetrics(r)
		c.estimate(cfg, r, &m)
		if m.PricedAttempts > 0 {
			t.Fatalf("priced tier %s", tier)
		}
	}
}

func TestCatalogHTTPBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{{"success", 200, testCatalog}, {"status", 503, testCatalog}, {"too large", 200, strings.Repeat("x", maxCatalogBytes+1)}} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("credentials sent")
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL, nil)
			client := catalogClient()
			defer client.CloseIdleConnections()
			raw, err := downloadCatalog(client, req)
			if tt.name == "success" {
				if err != nil || string(raw) != testCatalog {
					t.Fatal("download failed")
				}
			} else if err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
	client := catalogClient()
	defer client.CloseIdleConnections()
	if client.Timeout != 10*time.Second || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("transport limits")
	}
	for _, url := range []string{"http://models.dev/api.json", "https://evil.test/api.json", "https://models.dev:444/api.json", "https://user:secret@models.dev/api.json"} {
		req, _ := http.NewRequest("GET", url, nil)
		if client.CheckRedirect(req, nil) == nil {
			t.Fatalf("accepted redirect %s", url)
		}
	}
}

func TestCatalogDimensionLimits(t *testing.T) {
	for _, n := range []int{513, 20001} {
		entries := map[string]any{}
		for i := 0; i < n; i++ {
			entries[fmt.Sprint("x", i)] = map[string]any{"cost": map[string]any{"input": 1}}
		}
		var v any = entries
		if n == 20001 {
			v = map[string]any{"openai": map[string]any{"models": entries}}
		}
		raw, _ := json.Marshal(v)
		if _, err := parseCatalog(raw); err == nil {
			t.Fatalf("accepted dimension %d", n)
		}
	}
}

func TestLiveModelsDevCatalog(t *testing.T) {
	if os.Getenv("MODELS_DEV_LIVE") != "1" {
		t.Skip("explicit opt-in: anonymous public catalog only")
	}
	raw, err := fetchCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := json.Marshal(c)
	if err != nil || len(normalized) > maxNormalizedBytes {
		t.Fatal("normalized size", err)
	}
	models := 0
	for _, p := range c.Models {
		models += len(p)
	}
	m, ok := c.Models["openai"]["gpt-4o-mini"]
	if !ok || m.Rates.Input == nil || m.Rates.Output == nil || m.Unsupported {
		t.Fatal("expected public model missing")
	}
	t.Logf("public HTTPS bytes=%d normalized_bytes=%d providers=%d models=%d sha256=%s", len(raw), len(normalized), len(c.Models), models, c.Version)
}

func TestPricingWorkerCancelAndConcurrentLifecycle(t *testing.T) {
	entered := make(chan struct{}, 16)
	exited := make(chan struct{}, 16)
	fetch := func(ctx context.Context) ([]byte, error) {
		entered <- struct{}{}
		<-ctx.Done()
		exited <- struct{}{}
		return nil, ctx.Err()
	}
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "data"))
	s, err := openStore(cfg, fetch)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no startup worker")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				now := time.Now()
				_ = s.Ingest(context.Background(), event(fmt.Sprintf("%d-%d", i, j), "key", now.Add(-time.Second)), now)
				_, _ = s.Query(context.Background(), Query{}, now)
			}
		}(i)
	}
	cfg.PricingEnabled = false
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("disable did not join fetch")
	}
	cfg.PricingEnabled = true
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no re-enabled worker")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	select {
	case <-exited:
	default:
		t.Fatal("close did not join fetch")
	}
}

func TestPricingMigrationV2AndVersionRetention(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "data"))
	cfg.PricingEnabled = false
	s, err := openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ingest(t, s, event("old", "client", now.Add(-time.Minute)), now)
	before := query(t, s, Query{}, now)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(cfg.Directory, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE events SET metrics=json_remove(metrics,'$.priced_attempts','$.savings_priced_attempts','$.estimated_cost_usd','$.estimated_cache_savings_usd'); DROP INDEX events_price_version; ALTER TABLE events DROP COLUMN price_version; DROP TABLE price_versions; DROP TABLE price_catalog; DELETE FROM meta WHERE key='pricing_error'; PRAGMA user_version=2;`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	cfg.PricingEnabled = true
	s, err = openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after := query(t, s, Query{}, now)
	if !reflect.DeepEqual(before.Clients, after.Clients) || after.Totals.EstimatedCostUSD != nil || after.Pricing.UnpricedAttempts != 1 {
		t.Fatal("migration changed identity/history")
	}
	refresh := func(n int) {
		t.Helper()
		raw := strings.ReplaceAll(testCatalog, `"input":2`, fmt.Sprintf(`"input":%d`, n))
		if err := s.refreshPricing(context.Background(), func(context.Context) ([]byte, error) { return []byte(raw), nil }, now); err != nil {
			t.Fatal(err)
		}
	}
	refresh(2)
	ingest(t, s, event("priced", "client", now.Add(-time.Minute)), now)
	for i := 3; i < 10; i++ {
		refresh(i)
	}
	var catalogs, versions int
	if err := s.db.QueryRow("SELECT (SELECT COUNT(*) FROM price_catalog),(SELECT COUNT(*) FROM price_versions)").Scan(&catalogs, &versions); err != nil {
		t.Fatal(err)
	}
	if catalogs != 1 || versions != 2 {
		t.Fatalf("cache grew: %d catalogs %d versions", catalogs, versions)
	}
	if err := s.Maintain(now.AddDate(0, 0, 91)); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow("SELECT COUNT(*) FROM price_versions").Scan(&versions)
	if versions != 1 {
		t.Fatalf("expired version leaked: %d", versions)
	}
}

func TestPricingUnknownExtensionsAndEmptyCatalog(t *testing.T) {
	for _, raw := range []string{`{"openai":{"models":{}}}`, `{"openai":{"models":{"model":{"cost":{"input":2,"tiers":[{"tier":{"type":"context","size":10,"service":"premium"},"input":4}]}}}}}`} {
		c, err := parseCatalog([]byte(raw))
		if err != nil {
			continue
		}
		r := event("x", "key", time.Now())
		r.Detail = pluginapi.UsageDetail{InputTokens: 20}
		m := tokenMetrics(r)
		c.estimate(DefaultConfig("/unused"), r, &m)
		if len(c.Models["openai"]) == 0 || m.PricedAttempts > 0 {
			t.Error("accepted empty/ambiguous catalog")
		}
	}
}

func TestResolvedServiceTierAndRefreshGateFailure(t *testing.T) {
	c, err := parseCatalog([]byte(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	r := event("tier", "key", time.Now())
	r.ServiceTier = "auto"
	r.ResponseServiceTier = "default"
	m := tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	if m.PricedAttempts != 1 {
		t.Fatal("authoritative response tier not used")
	}
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "data"))
	s, err := openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.acquire(context.Background())
	err = s.refreshPricing(context.Background(), func(context.Context) ([]byte, error) { return []byte(testCatalog), nil }, time.Now())
	s.release()
	if err == nil || query(t, s, Query{}, time.Now()).Pricing.Status != "error" {
		t.Fatal("refresh storage failure not reported")
	}
}

func TestPricingStatusSavingsAndWhitelist(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "data"))
	s, err := openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if d := query(t, s, Query{}, now); d.Pricing.Status != "loading" || d.Pricing.SnapshotVersion != nil || d.Pricing.SnapshotVersions == nil {
		t.Fatal("initial status")
	}
	fetch := func(context.Context) ([]byte, error) {
		return []byte(`{"openai":{"env":["secret-canary"],"api":"https://private-canary.invalid","models":{"model":{"description":"prompt-canary","cost":{"output":0}}}}}`), nil
	}
	if err := s.refreshPricing(context.Background(), fetch, now); err != nil {
		t.Fatal(err)
	}
	r := event("zero-price", "client", now.Add(-time.Minute))
	r.Detail = pluginapi.UsageDetail{OutputTokens: 20}
	ingest(t, s, r, now)
	d := query(t, s, Query{}, now)
	if !d.Pricing.Available || d.Pricing.EstimatedCostUSD == nil || *d.Pricing.EstimatedCostUSD != 0 || d.Pricing.EstimatedCacheSavingsUSD != nil || d.Pricing.SavingsPricedAttempts != 0 {
		t.Fatalf("cost/savings coverage %+v", d.Pricing)
	}
	if d := query(t, s, Query{}, now.Add(25*time.Hour)); d.Pricing.Status != "stale" || d.Pricing.Available || d.Pricing.EstimatedCostUSD != nil {
		t.Fatal("age/window status")
	}
	cfg.PricingEnabled = false
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	if d := query(t, s, Query{}, now); d.Pricing.Status != "disabled" || !d.Pricing.Available {
		t.Fatal("disabled hides historical price")
	}
	r.RequestID = "disabled"
	ingest(t, s, r, now)
	if d := query(t, s, Query{}, now); d.Pricing.PricedAttempts != 1 || d.Pricing.UnpricedAttempts != 1 {
		t.Fatal("disabled priced new event")
	}
	var raw []byte
	if err := s.db.QueryRow("SELECT catalog FROM price_catalog").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"secret-canary", "private-canary", "prompt-canary"} {
		if bytes.Contains(raw, []byte(canary)) {
			t.Fatal("non-whitelisted catalog metadata persisted")
		}
	}
}

func TestRefreshDuringIngestReloadAndClose(t *testing.T) {
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "data"))
	s, err := openStore(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = s.refreshPricing(context.Background(), func(context.Context) ([]byte, error) { return []byte(testCatalog), nil }, time.Now())
			if i == 4 {
				close(refreshed)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			now := time.Now()
			_ = s.Ingest(context.Background(), event(fmt.Sprint(i), "key", now.Add(-time.Second)), now)
			_, _ = s.Query(context.Background(), Query{}, now)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_ = s.Update(cfg)
		}
	}()
	<-refreshed
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func TestCatalogHTTPRedirectAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://credentials-canary.invalid/", 302)
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := catalogClient()
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("GET", server.URL+"/redirect", nil)
	if _, err := downloadCatalog(client, req); err == nil {
		t.Fatal("followed foreign redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	done := make(chan error, 1)
	go func() { _, err := downloadCatalog(client, req); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "canary") {
			t.Fatal("cancellation/error redaction")
		}
	case <-time.After(time.Second):
		t.Fatal("fetch did not cancel")
	}
}

func TestTierMissingRatesAreUnknownAndLegacyDoesNotOverride(t *testing.T) {
	c, err := parseCatalog([]byte(`{"openai":{"models":{"model":{"cost":{"input":2,"output":8,"cache_read":0.5,"tiers":[{"tier":{"type":"context","size":272000},"input":4}],"context_over_200k":{"input":4,"output":16,"cache_read":1}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := event("tier", "key", time.Now())
	r.Detail = pluginapi.UsageDetail{InputTokens: 250000}
	m := tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	if m.EstimatedCostUSD == nil || *m.EstimatedCostUSD != .5 {
		t.Fatal("legacy wrongly overrides real threshold")
	}
	r.Detail = pluginapi.UsageDetail{InputTokens: 272001, OutputTokens: 1}
	m = tokenMetrics(r)
	c.estimate(DefaultConfig("/unused"), r, &m)
	if m.EstimatedCostUSD != nil {
		t.Fatal("selected tier's missing positive component rate was invented")
	}
}
