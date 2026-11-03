//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/local/cli-proxy-usage-meter/internal/meter"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

const managementKey = "synthetic-management-password"
const clientA = "synthetic-client-key-alpha"
const clientB = "synthetic-client-key-beta"

type capture chan usage.Record

func (c capture) HandleUsage(_ context.Context, r usage.Record) {
	select {
	case c <- r:
	default:
	}
}

// Each phase is a new OS process, proving SQLite identity/dedupe persistence
// across an actual host/plugin restart rather than merely reopening a Go object.
func TestOfficialRuntime(t *testing.T) {
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	if err := os.Mkdir(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	artifact, err := filepath.Abs("../build/client-usage-meter.so")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("build the native plugin first: %v", err)
	}
	if err = os.WriteFile(filepath.Join(plugins, "client-usage-meter.so"), b, 0700); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"seed", "restart", "priced", "priced-restart", "disabled"} {
		if !t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "METER_RUNTIME_PHASE="+phase, "METER_RUNTIME_ROOT="+root)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("official host phase failed: %v\n%s", err, out)
			}
			t.Logf("%s", out)
		}) {
			break
		}
	}
}
func TestRuntimeProcess(t *testing.T) {
	log.SetLevel(log.ErrorLevel)
	phase := os.Getenv("METER_RUNTIME_PHASE")
	if phase == "" {
		t.Skip("subprocess helper")
	}
	root := os.Getenv("METER_RUNTIME_ROOT")
	for _, name := range []string{"MANAGEMENT_PASSWORD", "GITHUB_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(name, "")
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte("synthetic-fail")) {
			w.WriteHeader(500)
			io.WriteString(w, `{"error":{"message":"synthetic upstream failure"}}`)
			return
		}
		io.WriteString(w, `{"id":"synthetic-completion","object":"chat.completion","model":"meter-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"synthetic response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40}}}`)
	}))
	defer stub.Close()
	// Background npm lookups are sent to a local rejecting proxy. Catalog reads
	// use local files. Pricing alone fetches the public models.dev catalog.
	// Inference credentials explicitly use the loopback stub.
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline integration", 502) }))
	defer reject.Close()
	catalog := filepath.Join(root, "empty-catalog.json")
	if err := os.WriteFile(catalog, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := &config.Config{Host: "127.0.0.1", Port: port, AuthDir: filepath.Join(root, "auths"), CommercialMode: true, UsageStatisticsEnabled: false}
	if phase != "disabled" {
		hash, e := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.MinCost)
		if e != nil {
			t.Fatal(e)
		}
		cfg.RemoteManagement.SecretKey = string(hash)
	}
	cfg.APIKeys = []string{clientA, clientB}
	if phase == "restart" {
		cfg.APIKeys = []string{clientB}
	}
	cfg.ProxyURL = reject.URL
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.RequestRetry = 0
	cfg.DisableCooling = true
	cfg.ErrorLogsMaxFiles = 0
	cfg.Models = config.ModelCatalogs{Catalog: catalog, CodexCatalog: catalog, DevinCatalog: catalog}
	cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "meter-stub", BaseURL: stub.URL + "/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-upstream-key", ProxyURL: "direct"}}, Models: []config.OpenAICompatibilityModel{{Name: "meter-test-model", Alias: "meter-test-model"}, {Name: "gpt-4o-mini", Alias: "gpt-4o-mini"}}}}
	pluginYAML := fmt.Sprintf("plugins:\n  enabled: true\n  dir: %q\n  configs:\n    client-usage-meter:\n      enabled: true\n      data_dir: %q\n      pricing_enabled: %t\n      pricing_provider_map: '{\"openai-compatible-meter-stub\":\"openai\"}'\n", filepath.Join(root, "plugins"), filepath.Join(root, "data"), strings.HasPrefix(phase, "priced"))
	if err = yaml.Unmarshal([]byte(pluginYAML), cfg); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config-"+phase+".yaml")
	raw, _ := yaml.Marshal(cfg)
	if err = os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	builder := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(cfgPath)
	if phase != "disabled" {
		builder.WithLocalManagementPassword(managementKey)
	}
	service, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	captured := make(capture, 32)
	usage.RegisterNamedPlugin("integration-independent-observer", captured)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case e := <-done:
			if e != nil && !errors.Is(e, context.Canceled) {
				t.Error(e)
			}
		case <-time.After(10 * time.Second):
			t.Error("host shutdown timed out")
		}
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, e := client.Get(base + "/")
		if e == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not start", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	request := func(method, path, key, body string) (int, []byte) {
		t.Helper()
		req, e := http.NewRequest(method, base+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, e := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		if e != nil {
			t.Fatal(e)
		}
		return res.StatusCode, b
	}
	for _, path := range []string{"/v0/resource/plugins/client-usage-meter/client-usage-meter", "/client-usage-meter", "/v1/client-usage-meter"} {
		status, _ := request("GET", path, "", "")
		if status != 404 {
			t.Fatalf("public route %s: %d", path, status)
		}
	}
	if phase == "disabled" {
		status, _ := request("GET", meter.Route, managementKey, "")
		if status != 404 {
			t.Fatalf("disabled management status %d", status)
		}
		return
	}
	for _, key := range []string{"", "wrong", clientA} {
		status, _ := request("GET", meter.Route, key, "")
		if status != 401 && status != 403 {
			t.Fatalf("auth denied status: %d", status)
		}
		status, _ = request("PUT", meter.ClientLabelRoute, key, `{}`)
		if status != 401 && status != 403 {
			t.Fatalf("label update auth denied status: %d", status)
		}
		// A successful management read resets the host's failed-auth counter;
		// keep probing the routes without triggering its IP lockout policy.
		if status, _ := request("GET", meter.Route, managementKey, ""); status != 200 {
			t.Fatalf("authenticated read: %d", status)
		}
	}
	read := func() meter.Response {
		t.Helper()
		status, b := request("GET", meter.Route, managementKey, "")
		if status != 200 {
			t.Fatalf("meter route: %d %s", status, b)
		}
		for _, secret := range []string{clientA, clientB, "synthetic-upstream-key", "synthetic response", "synthetic prompt"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatal("secret in response")
			}
		}
		var data meter.Response
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	wait := func(attempts, dups int64) meter.Response {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			r := read()
			if r.Totals.Attempts == attempts && r.Collection.Duplicates == dups {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected attempts=%d duplicates=%d; got totals=%+v pricing=%+v", attempts, dups, r.Totals, r.Pricing)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if phase == "priced" {
		deadline := time.Now().Add(15 * time.Second)
		for {
			d := read()
			if d.Pricing.Status == "ready" {
				break
			}
			if d.Pricing.Status == "error" || time.Now().After(deadline) {
				t.Fatalf("public catalog unavailable: %+v", d.Pricing)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if d := read(); d.Pricing.Available || d.Pricing.UnpricedAttempts != 3 {
			t.Fatal("old events repriced")
		}
		for _, key := range []string{clientA, clientB} {
			status, b := request("POST", "/v1/chat/completions", key, `{"model":"gpt-4o-mini","service_tier":"default","messages":[{"role":"user","content":"synthetic prompt"}]}`)
			if status != 200 {
				t.Fatalf("priced inference: %d %s", status, b)
			}
		}
		d := wait(5, 2)
		var replay usage.Record
		for i := 0; i < 2; i++ {
			select {
			case replay = <-captured:
				t.Logf("pricing callback provider=%q executor=%q model=%q tiers=%q/%q/%q", replay.Provider, replay.ExecutorType, replay.Model, replay.ServiceTier, replay.EffectiveServiceTier, replay.ResponseServiceTier)
			case <-time.After(time.Second):
				t.Fatal("priced callback absent")
			}
		}
		if d.Pricing.PricedAttempts != 2 || d.Pricing.UnpricedAttempts != 3 || d.Pricing.EstimatedCostUSD == nil || *d.Pricing.EstimatedCostUSD <= 0 || d.Pricing.EstimatedCacheSavingsUSD == nil || *d.Pricing.EstimatedCacheSavingsUSD <= 0 || len(d.Pricing.SnapshotVersions) != 1 {
			t.Fatalf("native priced callbacks: %+v", d.Pricing)
		}
		usage.PublishRecord(context.Background(), replay)
		wait(5, 3)
		replay.RequestID = "synthetic-premium-tier"
		replay.ServiceTier = "priority"
		replay.EffectiveServiceTier = "priority"
		replay.ResponseServiceTier = "priority"
		usage.PublishRecord(context.Background(), replay)
		after := wait(6, 3)
		if after.Pricing.PricedAttempts != 2 || after.Pricing.UnpricedAttempts != 4 || *after.Pricing.EstimatedCostUSD != *d.Pricing.EstimatedCostUSD {
			t.Fatalf("tier/dedupe pricing: %+v", after.Pricing)
		}
		var sum float64
		for _, c := range after.Clients {
			status, b := request("GET", meter.Route+"?client="+c.ID, managementKey, "")
			var part meter.Response
			if status != 200 || json.Unmarshal(b, &part) != nil {
				t.Fatal("priced client filter")
			}
			if part.Pricing.EstimatedCostUSD != nil {
				sum += *part.Pricing.EstimatedCostUSD
			}
		}
		if math.Abs(sum-*after.Pricing.EstimatedCostUSD) > 1e-12 {
			t.Fatal("priced clients do not reconcile")
		}
		b, _ := json.Marshal(after.Pricing)
		if err := os.WriteFile(filepath.Join(root, "prices.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		if snapshot := os.Getenv("METER_RUNTIME_PRICED_SNAPSHOT"); snapshot != "" {
			_, wire := request("GET", meter.Route, managementKey, "")
			if err := os.WriteFile(snapshot, wire, 0600); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("real models.dev snapshot=%s fetched=%s priced=%d unpriced=%d cost=%.9f savings=%.9f", *after.Pricing.SnapshotVersion, *after.Pricing.FetchedAt, after.Pricing.PricedAttempts, after.Pricing.UnpricedAttempts, *after.Pricing.EstimatedCostUSD, *after.Pricing.EstimatedCacheSavingsUSD)
	} else if phase == "priced-restart" {
		d := wait(6, 3)
		previous, err := os.ReadFile(filepath.Join(root, "prices.json"))
		if err != nil {
			t.Fatal(err)
		}
		current, _ := json.Marshal(d.Pricing)
		if !bytes.Equal(previous, current) {
			t.Fatalf("priced restart changed estimates: %s %s", previous, current)
		}
	} else if phase == "seed" {
		if read().Totals.Attempts != 0 {
			t.Fatal("unexpected backfill")
		}
		for _, pair := range []struct {
			key, prompt string
			status      int
		}{{clientA, "synthetic prompt", 200}, {clientB, "synthetic prompt", 200}, {clientA, "synthetic-fail", 500}} {
			body := fmt.Sprintf(`{"model":"meter-test-model","messages":[{"role":"user","content":%q}]}`, pair.prompt)
			status, b := request("POST", "/v1/chat/completions", pair.key, body)
			if status != pair.status {
				t.Fatalf("inference: %d %s", status, b)
			}
		}
		r := wait(3, 0)
		if r.Totals.TotalTokens != 240 || r.Totals.Succeeded != 2 || r.Totals.Failed != 1 || len(r.Clients) != 2 || r.Totals.Requests != nil {
			t.Fatalf("real inference accounting: %+v", r)
		}
		var replay usage.Record
		for i := 0; i < 3; i++ {
			select {
			case record := <-captured:
				if !record.Failed {
					replay = record
				}
			case <-time.After(time.Second):
				t.Fatal("independent observer did not receive callbacks")
			}
		}
		if replay.RequestID == "" {
			t.Fatal("host did not publish execution identity")
		}
		usage.PublishRecord(context.Background(), replay)
		r = wait(3, 1)
		labelBody, _ := json.Marshal(map[string]string{"client": r.Clients[0].ID, "name": "Runtime team"})
		if status, body := request("PUT", meter.ClientLabelRoute, managementKey, string(labelBody)); status != 200 {
			t.Fatalf("label update: %d %s", status, body)
		}
		if status, _ := request("PUT", meter.ClientLabelRoute, managementKey, `{"client":"invalid","name":"Team"}`); status != 400 {
			t.Fatalf("invalid label status: %d", status)
		}
		r = read()
		if r.Clients[0].Name != "Runtime team" || r.Clients[0].Label != "Runtime team" || len(r.ClientUsage) != 2 {
			t.Fatalf("key metadata: %+v", r.Clients)
		}
		// Optional sanitized wire snapshot for the sibling frontend contract smoke test.
		// This is generated only by isolated synthetic integration traffic, not production.
		if snapshot := os.Getenv("METER_RUNTIME_SNAPSHOT"); snapshot != "" {
			status, wire := request("GET", meter.Route, managementKey, "")
			if status != http.StatusOK {
				t.Fatalf("snapshot request failed: %d", status)
			}
			if err := os.WriteFile(snapshot, wire, 0600); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(replay)
		if err = os.WriteFile(filepath.Join(root, "synthetic-replay.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		b, _ = json.Marshal(r.Clients)
		if err = os.WriteFile(filepath.Join(root, "clients.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if read().Totals.Attempts != 3 {
				t.Fatal("management reads consumed data")
			}
		}
		var sum int64
		for _, c := range r.Clients {
			status, b := request("GET", meter.Route+"?client="+c.ID, managementKey, "")
			var part meter.Response
			if status != 200 || json.Unmarshal(b, &part) != nil {
				t.Fatal("filter failed")
			}
			sum += part.Totals.Attempts
		}
		if sum != 3 {
			t.Fatal("two-key reconciliation failed")
		}
	} else {
		r := wait(3, 1)
		b, _ := os.ReadFile(filepath.Join(root, "clients.json"))
		var previous []meter.Client
		json.Unmarshal(b, &previous)
		current, _ := json.Marshal(r.Clients)
		if !bytes.Equal(b, current) {
			t.Fatal("stable identities/history lost after key deletion and process restart")
		}
		b, err := os.ReadFile(filepath.Join(root, "synthetic-replay.json"))
		if err != nil {
			t.Fatal(err)
		}
		var replay usage.Record
		if json.Unmarshal(b, &replay) != nil {
			t.Fatal("replay decode")
		}
		usage.PublishRecord(context.Background(), replay)
		wait(3, 2)
	}
	for _, path := range []string{meter.Route + "?range=all", meter.Route + "?range=24h&range=7d", meter.Route + "?unknown=x"} {
		status, _ := request("GET", path, managementKey, "")
		if status != 400 {
			t.Fatalf("invalid query %s: %d", path, status)
		}
	}
	status, _ := request("POST", meter.Route, managementKey, "")
	if status != 404 && status != 405 {
		t.Fatalf("write route exposed: %d", status)
	}
	t.Log("compiled C ABI plugin, official SDK host, actual inference + usage fanout, management auth and public-route isolation verified")
}
