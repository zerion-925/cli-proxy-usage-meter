// Package plugin implements the official JSON C ABI methods without host callbacks.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/local/cli-proxy-usage-meter/internal/meter"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const MaxBody = 1 << 20
const queueSize = 256

type item struct {
	record  pluginapi.UsageRecord
	barrier chan struct{}
}
type Plugin struct {
	mu         sync.RWMutex
	store      *meter.Store
	queue      chan item
	stop, done chan struct{}
}

func New() *Plugin { return &Plugin{} }
func ok(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		return fail()
	}
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: b})
	return out
}
func fail() []byte {
	b, _ := pluginabi.NewErrorEnvelope("usage_meter_error", "usage meter operation unavailable")
	return b
}
func (p *Plugin) Dropped() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.store != nil {
		p.store.Drop(false)
	}
}

// Call never logs its input or returns parser/storage errors, which may contain secrets.
func (p *Plugin) Call(method string, raw []byte) []byte {
	if len(raw) > MaxBody {
		if method == pluginabi.MethodUsageHandle {
			p.Dropped()
			return ok(struct{}{})
		}
		return fail()
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if p.configure(raw) != nil {
			return fail()
		}
		return ok(registration())
	case pluginabi.MethodPluginQuiesce:
		return ok(struct{}{})
	case pluginabi.MethodPluginShutdown:
		p.Close()
		return ok(struct{}{})
	case pluginabi.MethodManagementRegister:
		return ok(struct {
			Routes []pluginapi.ManagementRoute `json:"routes"`
		}{[]pluginapi.ManagementRoute{{Method: "GET", Path: meter.Route}, {Method: "PUT", Path: meter.ClientLabelRoute}}})
	case pluginabi.MethodUsageHandle:
		// Deliberately decode only fields needed for aggregation. Headers, failures,
		// trace/session/auth IDs, URLs and other payloads never enter our queue.
		var r struct {
			RequestID, Provider, ExecutorType, Model, APIKey, AccessProvider string
			ServiceTier, EffectiveServiceTier, ResponseServiceTier           string
			IsNativeKey, Failed                                              bool
			RequestedAt                                                      time.Time
			Detail                                                           pluginapi.UsageDetail
		}
		if json.Unmarshal(raw, &r) != nil || len(r.RequestID) > 256 || len(r.APIKey) > 4096 || len(r.AccessProvider) > 256 || len(r.Provider) > 128 || len(r.ExecutorType) > 128 || len(r.Model) > 128 || len(r.ServiceTier) > 32 || len(r.EffectiveServiceTier) > 32 || len(r.ResponseServiceTier) > 32 {
			p.Dropped()
			return ok(struct{}{})
		}
		record := pluginapi.UsageRecord{ServiceTier: r.ServiceTier, EffectiveServiceTier: r.EffectiveServiceTier, ResponseServiceTier: r.ResponseServiceTier, RequestID: r.RequestID, Provider: r.Provider, ExecutorType: r.ExecutorType, Model: r.Model, APIKey: r.APIKey, AccessProvider: r.AccessProvider, IsNativeKey: r.IsNativeKey, Failed: r.Failed, RequestedAt: r.RequestedAt, Detail: r.Detail}
		p.mu.RLock()
		defer p.mu.RUnlock()
		if p.store != nil {
			select {
			case p.queue <- item{record: record}:
			default:
				p.store.Drop(false)
			}
		}
		return ok(struct{}{})
	case pluginabi.MethodManagementHandle:
		var r struct {
			Method, Path string
			Query        url.Values
			Body         []byte
		}
		if json.Unmarshal(raw, &r) != nil {
			return ok(httpResult(400, nil))
		}
		if r.Path == meter.ClientLabelRoute {
			if r.Method != "PUT" {
				return ok(httpResult(405, nil))
			}
			var label struct {
				Client string `json:"client"`
				Name   string `json:"name"`
			}
			decoder := json.NewDecoder(bytes.NewReader(r.Body))
			decoder.DisallowUnknownFields()
			if len(r.Query) != 0 || len(r.Body) > 1024 || decoder.Decode(&label) != nil || !meter.ValidClientName(label.Client, label.Name) || decoder.Decode(new(any)) != io.EOF {
				return ok(httpResult(400, nil))
			}
			p.mu.RLock()
			defer p.mu.RUnlock()
			if p.store == nil {
				return ok(httpResult(503, nil))
			}
			err := p.store.SetClientName(context.Background(), label.Client, label.Name)
			if errors.Is(err, meter.ErrClientNotFound) {
				return ok(httpResult(404, nil))
			}
			if err != nil {
				return ok(httpResult(503, nil))
			}
			return ok(httpResult(200, map[string]bool{"saved": true}))
		}
		if r.Path != meter.Route {
			return ok(httpResult(404, nil))
		}
		if r.Method != "GET" {
			return ok(httpResult(405, nil))
		}
		if len(r.Body) > 0 || !validQuery(r.Query) {
			return ok(httpResult(400, nil))
		}
		q := meter.Query{Range: r.Query.Get("range"), Client: r.Query.Get("client"), Bucket: r.Query.Get("bucket")}
		if _, _, err := q.Window(time.Now()); err != nil {
			return ok(httpResult(400, nil))
		}
		p.mu.RLock()
		defer p.mu.RUnlock()
		if p.store == nil {
			return ok(httpResult(503, nil))
		}
		data, err := p.store.Query(context.Background(), q, time.Now().UTC())
		if err != nil {
			return ok(httpResult(503, nil))
		}
		return ok(httpResult(200, data))
	default:
		return fail()
	}
}
func validQuery(q url.Values) bool {
	if len(q) > 3 {
		return false
	}
	for k, v := range q {
		if (k != "range" && k != "client" && k != "bucket") || len(v) != 1 || len(v[0]) > 80 {
			return false
		}
	}
	return true
}
func httpResult(status int, data any) pluginapi.ManagementResponse {
	if data == nil {
		data = map[string]string{"error": http.StatusText(status)}
	}
	b, _ := json.Marshal(data)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, Body: b}
}
func registration() any {
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}{pluginabi.SchemaVersion, pluginapi.Metadata{Name: "client-usage-meter", Version: "1.2.0", Author: "Usage Meter contributors", GitHubRepository: "https://github.com/zerion-925/cli-proxy-usage-meter", ConfigFields: []pluginapi.ConfigField{
		{Name: "pricing_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Fetch anonymous public models.dev prices (default true). Disabling preserves stored estimates."},
		{Name: "pricing_provider_map", Type: pluginapi.ConfigFieldTypeString, Description: "JSON object (or YAML mapping): exact native provider ID to models.dev provider ID; no URLs or credentials."},
		{Name: "pricing_model_map", Type: pluginapi.ConfigFieldTypeString, Description: "JSON object (or YAML mapping): native-provider/event-model to exact catalog model ID."},
		{Name: "data_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Absolute dedicated directory, owned by this user with mode 0700. Changes require restart."}, {Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Retention 1–365 days (default 90)."}, {Name: "max_events", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum retained attempts 1–1000000 (default 100000)."}, {Name: "max_clients", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum retained clients 1–1000 (default 1000)."}, {Name: "max_models", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum retained model names 1–256 (default 256)."}}}, map[string]bool{"usage_plugin": true, "management_api": true}}
}
func (p *Plugin) configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if json.Unmarshal(raw, &req) != nil || len(req.ConfigYAML) > 16384 {
		return errors.New("invalid configuration")
	}
	cfg := meter.DefaultConfig(os.Getenv("CLI_PROXY_USAGE_METER_DIR"))
	if yaml.Unmarshal(req.ConfigYAML, &cfg) != nil {
		return errors.New("invalid configuration")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store != nil {
		return p.store.Update(cfg)
	}
	s, err := meter.Open(cfg)
	if err != nil {
		return err
	}
	p.store = s
	p.queue = make(chan item, queueSize)
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go p.worker(s, p.queue, p.stop, p.done)
	return nil
}
func (p *Plugin) worker(s *meter.Store, q <-chan item, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	process := func(v item) {
		if v.barrier != nil {
			close(v.barrier)
		} else {
			_ = s.Ingest(context.Background(), v.record, time.Now().UTC())
		}
	}
	for {
		select {
		case v := <-q:
			process(v)
		case <-ticker.C:
			if s.Maintain(time.Now().UTC()) != nil {
				s.MaintenanceFailure()
			}
		case <-stop:
			deadline := time.Now().Add(2 * time.Second)
			for {
				select {
				case v := <-q:
					if time.Now().Before(deadline) {
						process(v)
					} else if v.barrier != nil {
						close(v.barrier)
					} else {
						s.Drop(false)
					}
				default:
					return
				}
			}
		}
	}
}

// Flush waits for events accepted before its barrier. It is a local testing and shutdown helper.
func (p *Plugin) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.store == nil {
		return errors.New("disabled")
	}
	done := make(chan struct{})
	select {
	case p.queue <- item{barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *Plugin) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return
	}
	close(p.stop)
	<-p.done
	_ = p.store.Close()
	p.store = nil
}
