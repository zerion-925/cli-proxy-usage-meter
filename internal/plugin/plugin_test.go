package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/local/cli-proxy-usage-meter/internal/meter"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func invoke(t *testing.T, p *Plugin, method string, req any) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(req)
	out := p.Call(method, b)
	var e pluginabi.Envelope
	if json.Unmarshal(out, &e) != nil || !e.OK {
		t.Fatalf("%s: %s", method, out)
	}
	return e.Result
}
func TestLifecycleUsageAndPrivateRoute(t *testing.T) {
	p := New()
	defer p.Close()
	cfg := map[string]any{"config_yaml": []byte(fmt.Sprintf("pricing_enabled: false\ndata_dir: %q\n", filepath.Join(t.TempDir(), "data")))}
	invoke(t, p, "plugin.register", cfg)
	raw := invoke(t, p, "management.register", nil)
	var reg struct {
		Routes    []pluginapi.ManagementRoute
		Resources []pluginapi.ResourceRoute
	}
	if json.Unmarshal(raw, &reg) != nil || len(reg.Routes) != 2 || reg.Routes[1].Method != "PUT" || reg.Routes[1].Path != meter.ClientLabelRoute || reg.Routes[0].Menu != "" || len(reg.Resources) != 0 || reg.Routes[0].Path != meter.Route {
		t.Fatalf("routes: %s", raw)
	}
	r := pluginapi.UsageRecord{RequestID: "a", APIKey: "synthetic-secret", IsNativeKey: true, Provider: "openai", Model: "model", RequestedAt: time.Now().Add(-time.Second), Detail: pluginapi.UsageDetail{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}}
	invoke(t, p, "usage.handle", r)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		raw = invoke(t, p, "management.handle", pluginapi.ManagementRequest{Method: "GET", Path: meter.Route})
		var out pluginapi.ManagementResponse
		if json.Unmarshal(raw, &out) != nil || out.StatusCode != 200 {
			t.Fatalf("HTTP: %s", raw)
		}
		var data meter.Response
		if json.Unmarshal(out.Body, &data) != nil || data.Totals.Attempts != 1 {
			t.Fatalf("data: %s", out.Body)
		}
	}
	invoke(t, p, "plugin.reconfigure", cfg)
	invoke(t, p, "usage.handle", r)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw = invoke(t, p, "management.handle", pluginapi.ManagementRequest{Method: "GET", Path: meter.Route})
	var out pluginapi.ManagementResponse
	json.Unmarshal(raw, &out)
	var data meter.Response
	json.Unmarshal(out.Body, &data)
	if data.Totals.Attempts != 1 || data.Collection.Duplicates != 1 {
		t.Fatalf("reload data: %s", out.Body)
	}
}

func configured(t *testing.T) (*Plugin, map[string]any, string) {
	t.Helper()
	p := New()
	dir := filepath.Join(t.TempDir(), "data")
	cfg := map[string]any{"config_yaml": []byte(fmt.Sprintf("pricing_enabled: false\ndata_dir: %q\n", dir))}
	invoke(t, p, "plugin.register", cfg)
	t.Cleanup(p.Close)
	return p, cfg, dir
}
func readData(t *testing.T, p *Plugin) meter.Response {
	t.Helper()
	raw := invoke(t, p, "management.handle", pluginapi.ManagementRequest{Method: "GET", Path: meter.Route})
	var out pluginapi.ManagementResponse
	json.Unmarshal(raw, &out)
	if out.StatusCode != 200 {
		t.Fatalf("HTTP %d", out.StatusCode)
	}
	var data meter.Response
	if json.Unmarshal(out.Body, &data) != nil {
		t.Fatal("invalid JSON")
	}
	return data
}
func TestMalformedInputAndFailedReloadKeepWorking(t *testing.T) {
	p, _, _ := configured(t)
	for _, raw := range [][]byte{[]byte(`{"APIKey":"private-canary","Detail":"bad"}`), bytes.Repeat([]byte("x"), MaxBody+1)} {
		out := p.Call("usage.handle", raw)
		if bytes.Contains(out, []byte("private-canary")) {
			t.Fatal("input leaked")
		}
		var env pluginabi.Envelope
		if json.Unmarshal(out, &env) != nil || !env.OK {
			t.Fatal("usage errors should be acknowledged")
		}
	}
	invalid, _ := json.Marshal(map[string]any{"config_yaml": []byte("data_dir: relative-secret-path")})
	out := p.Call("plugin.reconfigure", invalid)
	var env pluginabi.Envelope
	json.Unmarshal(out, &env)
	if env.OK || bytes.Contains(out, []byte("relative-secret")) {
		t.Fatal("failed reload response")
	}
	if d := readData(t, p); d.Collection.DroppedEvents != 2 {
		t.Fatalf("drop count: %+v", d.Collection)
	}
	for _, r := range []pluginapi.ManagementRequest{{Method: "POST", Path: meter.Route}, {Method: "GET", Path: "/public"}, {Method: "GET", Path: meter.Route, Query: url.Values{"client": {"a", "b"}}}, {Method: "GET", Path: meter.Route, Body: []byte("private-body")}} {
		raw := invoke(t, p, "management.handle", r)
		var out pluginapi.ManagementResponse
		json.Unmarshal(raw, &out)
		if out.StatusCode < 400 || bytes.Contains(out.Body, []byte("private-body")) {
			t.Fatalf("bad management result %s", raw)
		}
	}
}
func TestConcurrentLifecycleAndBoundedQueue(t *testing.T) {
	p, cfg, dir := configured(t)
	db, err := sql.Open("sqlite3", filepath.Join(dir, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 800; i++ {
		r := pluginapi.UsageRecord{RequestID: fmt.Sprint(i), RequestedAt: time.Now().Add(-time.Second)}
		raw, _ := json.Marshal(r)
		p.Call("usage.handle", raw)
	}
	if time.Since(start) > time.Second {
		t.Fatal("callbacks blocked on storage")
	}
	db.Exec("ROLLBACK")
	if err = p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readData(t, p).Collection.DroppedEvents == 0 {
		t.Fatal("bounded queue did not report overflow")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, _ := json.Marshal(cfg)
			p.Call("plugin.reconfigure", raw)
			r := pluginapi.UsageRecord{RequestID: fmt.Sprintf("later-%d", i), RequestedAt: time.Now().Add(-time.Second)}
			raw, _ = json.Marshal(r)
			p.Call("usage.handle", raw)
		}(i)
	}
	wg.Wait()
	if err = p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.Close()
	invoke(t, p, "plugin.register", cfg)
	if readData(t, p).Collection.DroppedEvents == 0 {
		t.Fatal("reload lost health")
	}
}

func TestRejectedManagementInputDoesNotCountAsLostUsage(t *testing.T) {
	p, _, _ := configured(t)
	p.Call("management.handle", bytes.Repeat([]byte("x"), MaxBody+1))
	if readData(t, p).Collection.DroppedEvents != 0 {
		t.Fatal("management input counted as dropped usage")
	}
}

func TestPricingConfigMetadata(t *testing.T) {
	p := New()
	defer p.Close()
	raw := invoke(t, p, "plugin.register", map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\npricing_enabled: false\npricing_provider_map: '{\"custom\":\"openai\"}'\npricing_model_map:\n  custom/alias: gpt-4o-mini\n", filepath.Join(t.TempDir(), "data")))})
	var reg struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if json.Unmarshal(raw, &reg) != nil {
		t.Fatal("metadata")
	}
	fields := map[string]bool{}
	for _, f := range reg.Metadata.ConfigFields {
		fields[f.Name] = true
	}
	for _, name := range []string{"pricing_enabled", "pricing_provider_map", "pricing_model_map"} {
		if !fields[name] {
			t.Errorf("missing config field %s", name)
		}
	}
	d := readData(t, p)
	if d.Pricing.Status != "disabled" || d.Pricing.Available || d.Pricing.EstimatedCostUSD != nil {
		t.Fatalf("disabled response %+v", d.Pricing)
	}
}

func TestLegacyAttemptCapIsIgnoredOnRegisterAndReload(t *testing.T) {
	p := New()
	defer p.Close()
	dir := filepath.Join(t.TempDir(), "data")
	cfg := map[string]any{"config_yaml": []byte(fmt.Sprintf("pricing_enabled: false\ndata_dir: %q\nretention_days: 90\nmax_events: 1\n", dir))}
	registered := invoke(t, p, "plugin.register", cfg)
	if bytes.Contains(registered, []byte("max_events")) {
		t.Fatal("removed setting still advertised")
	}
	for i := 0; i < 3; i++ {
		if i == 2 {
			invoke(t, p, "plugin.reconfigure", cfg)
		}
		invoke(t, p, "usage.handle", pluginapi.UsageRecord{RequestID: fmt.Sprint(i), RequestedAt: time.Now().Add(-time.Second), Model: "model", Detail: pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}})
		if err := p.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := readData(t, p)
	if got.Totals.Attempts != 3 || got.Collection.DroppedEvents != 0 || got.Collection.RetentionDays != 90 {
		t.Fatalf("legacy limit affected retention: %+v", got)
	}
}
