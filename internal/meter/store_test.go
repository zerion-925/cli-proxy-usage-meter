package meter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRestartDeduplicatesAndPreservesIdentity(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC)
	cfg := testConfig(filepath.Join(t.TempDir(), "data"))
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := pluginapi.UsageRecord{RequestID: "execution-1", APIKey: "synthetic-client-secret", IsNativeKey: true, AccessProvider: "config-inline", Provider: "openai", Model: "test-model", RequestedAt: now.Add(-time.Minute), Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, TotalTokens: 120}}
	if err := s.Ingest(context.Background(), r, now); err != nil {
		t.Fatal(err)
	}
	a, err := s.Query(context.Background(), Query{Range: "24h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if a.Totals.Attempts != 1 || a.Totals.TotalTokens != 120 || a.Totals.Requests != nil {
		t.Fatalf("totals: %+v", a.Totals)
	}
	if len(a.Clients) != 1 || a.Clients[0].ID == r.APIKey {
		t.Fatalf("clients: %+v", a.Clients)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Ingest(context.Background(), r, now); err != nil {
		t.Fatal(err)
	}
	b, err := s.Query(context.Background(), Query{Range: "24h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.Totals.Attempts != 1 || b.Collection.Duplicates != 1 || b.Clients[0] != a.Clients[0] || b.Collection.StartedAt != a.Collection.StartedAt {
		t.Fatalf("restart: %+v", b)
	}
}

func openTest(t *testing.T) (*Store, time.Time) {
	t.Helper()
	s, err := Open(testConfig(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
}
func event(id, key string, at time.Time) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{RequestID: id, APIKey: key, IsNativeKey: true, AccessProvider: "config-inline", Provider: "openai", Model: "model", RequestedAt: at, Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, TotalTokens: 120}}
}
func ingest(t *testing.T, s *Store, r pluginapi.UsageRecord, now time.Time) {
	t.Helper()
	if err := s.Ingest(context.Background(), r, now); err != nil {
		t.Fatal(err)
	}
}
func query(t *testing.T, s *Store, q Query, now time.Time) Response {
	t.Helper()
	v, err := s.Query(context.Background(), q, now)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTwoKeyReconciliationNamespacesRetriesAndMissingIDs(t *testing.T) {
	s, now := openTest(t)
	r := event("one", "synthetic-key-a", now.Add(-time.Hour))
	ingest(t, s, r, now)
	r.RequestID = "retry"
	r.Failed = true
	ingest(t, s, r, now)
	ingest(t, s, r, now)
	r = event("two", "synthetic-key-b", now.Add(-time.Minute))
	ingest(t, s, r, now)
	r.RequestID = "plugin-one"
	r.IsNativeKey = false
	r.AccessProvider = "plugin-one"
	ingest(t, s, r, now)
	r.RequestID = "plugin-two"
	r.AccessProvider = "plugin-two"
	ingest(t, s, r, now)
	r.RequestID = "unattributed"
	r.APIKey = ""
	ingest(t, s, r, now)
	r.RequestID = ""
	ingest(t, s, r, now)
	all := query(t, s, Query{Range: "24h"}, now)
	if all.Totals.Attempts != 6 || all.Totals.Succeeded != 5 || all.Totals.Failed != 1 || len(all.Clients) != 5 || all.Collection.Duplicates != 1 || all.Collection.MissingExecutionIDs != 1 || all.Collection.DroppedEvents != 1 {
		t.Fatalf("result: %+v", all)
	}
	var summed Metrics
	for _, c := range all.Clients {
		part := query(t, s, Query{Range: "24h", Client: c.ID}, now)
		summed.add(part.Totals)
	}
	if summed.Attempts != all.Totals.Attempts || summed.TotalTokens != all.Totals.TotalTokens || summed.CacheRateDenominator != all.Totals.CacheRateDenominator {
		t.Fatalf("reconciliation: %+v / %+v", summed, all.Totals)
	}
	if query(t, s, Query{Range: "24h", Client: "native_00000000000000000000000000000000"}, now).Totals.Attempts != 0 {
		t.Fatal("unknown filter returned records")
	}
	// No live key registry exists: removal from host configuration cannot delete retained history.
	if len(query(t, s, Query{Range: "24h"}, now.Add(2*time.Hour)).Clients) != 5 {
		t.Fatal("historical identities disappeared")
	}
}

func TestProviderAccounting(t *testing.T) {
	cases := []struct {
		name, provider, executor            string
		d                                   pluginapi.UsageDetail
		total, denom, unknown, unclassified int64
		rate                                *float64
	}{
		{name: "openai subsets", provider: "openai", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5, CacheReadTokens: 40, CachedTokens: 40, TotalTokens: 120}, total: 120, denom: 100},
		{name: "claude independent", provider: "claude", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5, CacheReadTokens: 40, CacheCreationTokens: 10, TotalTokens: 170}, total: 170, denom: 150},
		{name: "gemini separate reasoning", provider: "gemini", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5, CacheReadTokens: 40, TotalTokens: 125}, total: 125, denom: 100},
		{name: "executor fallback", provider: "custom", executor: "OpenAICompatExecutor", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40}, total: 120, denom: 100},
		{name: "unknown provider", provider: "custom", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5, TotalTokens: 120}, total: 120, unknown: 1},
		{name: "zero presence lost", provider: "openai", unknown: 1},
		{name: "ambiguous cached", provider: "openai", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CachedTokens: 40, TotalTokens: 120}, total: 120, unknown: 1, unclassified: 40},
		{name: "inconsistent", provider: "openai", d: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 140, TotalTokens: 120}, total: 120, unknown: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, now := openTest(t)
			r := event("id", "key", now.Add(-time.Second))
			r.Provider = c.provider
			r.ExecutorType = c.executor
			r.Detail = c.d
			ingest(t, s, r, now)
			m := query(t, s, Query{}, now).Totals
			if m.TotalTokens != c.total || m.CacheRateDenominator != c.denom || m.UnknownTokenEvents != c.unknown || m.UnclassifiedCachedTokens != c.unclassified {
				t.Fatalf("metrics: %+v", m)
			}
			if c.denom > 0 {
				if m.CacheRate == nil || *m.CacheRate != float64(c.d.CacheReadTokens)/float64(c.denom) {
					t.Fatalf("rate: %+v", m)
				}
			} else if m.CacheRate != nil {
				t.Fatal("invented rate")
			}
		})
	}
}

func TestTimeBoundariesRetentionAndLimits(t *testing.T) {
	s, now := openTest(t)
	cfg := s.cfg
	cfg.RetentionDays = 1
	cfg.MaxEvents = 3
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{now.Add(-24*time.Hour - time.Nanosecond), now.Add(-24 * time.Hour), now.Add(-time.Nanosecond), now} {
		ingest(t, s, event(fmt.Sprint(i), "key", at), now)
	}
	r := query(t, s, Query{}, now)
	if r.Totals.Attempts != 2 || len(r.Timeline) != 25 || r.Collection.DroppedEvents != 1 {
		t.Fatalf("boundaries: %+v", r)
	}
	ingest(t, s, event("capacity", "key", now.Add(-time.Second)), now)
	if query(t, s, Query{}, now).Collection.DroppedEvents != 2 {
		t.Fatal("event cap not applied")
	}
	if err := s.Maintain(now.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r = query(t, s, Query{Range: "90d", Bucket: "hour"}, now.Add(48*time.Hour)); r.Totals.Attempts != 0 || len(r.Clients) != 0 || r.Collection.EarliestEventAt != nil || len(r.Timeline) > 2161 {
		t.Fatalf("retention: %+v", r)
	}
	for _, q := range []Query{{Range: "all"}, {Bucket: "minute"}, {Client: strings.Repeat("x", 81)}} {
		if _, err := s.Query(context.Background(), q, now); err == nil {
			t.Fatal("unbounded query accepted")
		}
	}
}

func TestConcurrentCallbacksAndReadReconciliation(t *testing.T) {
	s, now := openTest(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := event(fmt.Sprint(i), fmt.Sprintf("key-%d", i%2), now.Add(-time.Second))
			for j := 0; j < 2; j++ {
				if err := s.Ingest(context.Background(), r, now); err != nil {
					t.Error(err)
				}
			}
			if _, err := s.Query(context.Background(), Query{}, now); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	r := query(t, s, Query{}, now)
	if r.Totals.Attempts != 40 || r.Collection.Duplicates != 40 || len(r.Clients) != 2 {
		t.Fatalf("concurrency: %+v", r)
	}
}

func TestStorageFaultRedactionAndFileModes(t *testing.T) {
	s, now := openTest(t)
	secret := "synthetic-raw-secret-that-must-never-persist"
	r := event("secret-execution", secret, now.Add(-time.Second))
	r.Model = secret
	r.Failure.Body = "private-body-canary"
	r.ResponseHeaders = map[string][]string{"Authorization": {"Bearer private-header-canary"}}
	r.Source = "private-source-canary"
	ingest(t, s, r, now)
	response := query(t, s, Query{}, now)
	out, _ := json.Marshal(response)
	db, err := sql.Open("sqlite3", filepath.Join(s.cfg.Directory, "usage.sqlite")+"?_busy_timeout=100")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "fault"
	start := time.Now()
	err = s.Ingest(context.Background(), r, now)
	if err == nil || err.Error() != ErrStorage.Error() {
		t.Fatalf("fault: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("unbounded busy wait")
	}
	if _, err = db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	response = query(t, s, Query{}, now)
	if response.Collection.IngestionFailures != 1 || response.Collection.DroppedEvents != 1 || response.Totals.Attempts != 1 {
		t.Fatalf("fault health: %+v", response.Collection)
	}
	for _, n := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm", "writer.lock"} {
		p := filepath.Join(s.cfg.Directory, n)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode: %v", n, info.Mode())
		}
		for _, canary := range []string{secret, "private-body-canary", "private-header-canary", "private-source-canary", "secret-execution"} {
			if bytes.Contains(b, []byte(canary)) || bytes.Contains(out, []byte(canary)) {
				t.Fatalf("leaked %s", canary)
			}
		}
	}
	if _, err := Open(s.cfg); err == nil {
		t.Fatal("two writers acquired directory")
	}
	cfg := s.cfg
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if query(t, next, Query{}, now).Collection.IngestionFailures != 1 {
		t.Fatal("health not checkpointed")
	}
}

func TestRejectUnsafeStorageAndInvalidCounters(t *testing.T) {
	for _, dir := range []string{"relative", "/"} {
		if _, err := Open(testConfig(dir)); err == nil {
			t.Fatal("unsafe directory accepted")
		}
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testConfig(target)); err == nil {
		t.Fatal("public directory accepted")
	}
	if err := os.Chmod(target, 0700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "link")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testConfig(linked)); err == nil {
		t.Fatal("symlink directory accepted")
	}
	s, now := openTest(t)
	r := event("bad", "key", now.Add(-time.Second))
	r.Detail.TotalTokens = -1
	ingest(t, s, r, now)
	r.Detail.TotalTokens = 1000000001
	ingest(t, s, r, now)
	r.Detail.TotalTokens = 120
	r.RequestedAt = time.Time{}
	ingest(t, s, r, now)
	result := query(t, s, Query{}, now)
	if result.Totals.Attempts != 0 || result.Collection.DroppedEvents != 3 {
		t.Fatalf("invalid counters: %+v", result)
	}
}

func TestCardinalityRecyclesAfterRetentionAndLoweredLimitsPreserveReads(t *testing.T) {
	s, now := openTest(t)
	cfg := s.cfg
	cfg.MaxClients = 1
	cfg.MaxModels = 1
	cfg.RetentionDays = 1
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	ingest(t, s, event("a", "a", now.Add(-time.Second)), now)
	ingest(t, s, event("b", "b", now.Add(-time.Second)), now)
	r := event("c", "a", now.Add(-time.Second))
	r.Model = "another"
	ingest(t, s, r, now)
	if got := query(t, s, Query{}, now); got.Totals.Attempts != 1 || got.Collection.DroppedEvents != 2 {
		t.Fatalf("cardinality: %+v", got)
	}
	later := now.Add(48 * time.Hour)
	ingest(t, s, event("d", "d", later.Add(-time.Second)), later)
	ingest(t, s, event("e", "d", later.Add(-time.Second)), later)
	cfg.MaxEvents = 1
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	if got := query(t, s, Query{}, later); got.Totals.Attempts != 2 || len(got.Clients) != 1 {
		t.Fatalf("lowered limits truncated history: %+v", got)
	}
}

func TestSchemaMigrationPreservesHistoryAndRefusesFutureSchema(t *testing.T) {
	s, now := openTest(t)
	cfg := s.cfg
	ingest(t, s, event("persisted", "synthetic-key", now.Add(-time.Second)), now)
	before := query(t, s, Query{}, now)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(cfg.Directory, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DROP INDEX events_price_version; ALTER TABLE events DROP COLUMN price_version; DROP TABLE price_versions; DROP TABLE price_catalog; DROP TRIGGER events_insert; DROP TRIGGER events_delete; DROP TABLE client_dims; DROP TABLE model_dims; DROP TABLE event_count; PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	next, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	after := query(t, next, Query{}, now)
	if after.Totals.Attempts != 1 || after.Clients[0] != before.Clients[0] {
		t.Fatal("migration changed metrics or identity")
	}
	ingest(t, next, event("new", "synthetic-key", now.Add(-time.Second)), now)
	if query(t, next, Query{}, now).Totals.Attempts != 2 {
		t.Fatal("migration counters failed")
	}
	next.Close()
	db, err = sql.Open("sqlite3", filepath.Join(cfg.Directory, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA user_version=999")
	db.Close()
	if _, err = Open(cfg); err == nil {
		t.Fatal("future schema accepted")
	}
}

func TestClientFilterDoesNotEchoRawKeys(t *testing.T) {
	s, now := openTest(t)
	if _, err := s.Query(context.Background(), Query{Client: "synthetic-client-secret"}, now); err == nil {
		t.Fatal("raw key accepted as client filter")
	}
}

func TestCoverageBeforeCollectionOrRetentionIsPartial(t *testing.T) {
	s, _ := openTest(t)
	now := time.Now().UTC()
	got := query(t, s, Query{}, now)
	if got.Collection.State != "partial" {
		t.Fatal("pre-installation history must be partial")
	}
	later := now.Add(48 * time.Hour)
	got = query(t, s, Query{}, later)
	if got.Collection.State != "collecting" {
		t.Fatal("fully elapsed collection window should collect")
	}
	cfg := s.cfg
	cfg.RetentionDays = 1
	if err := s.Update(cfg); err != nil {
		t.Fatal(err)
	}
	got = query(t, s, Query{Range: "7d"}, later)
	if got.Collection.State != "partial" {
		t.Fatal("window beyond retention must be partial")
	}
}

func testConfig(dir string) Config { cfg := DefaultConfig(dir); cfg.PricingEnabled = false; return cfg }
