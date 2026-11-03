// Package meter stores only whitelisted, pseudonymous execution metrics.
package meter

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const Route = "/v0/management/client-usage-meter"
const ClientLabelRoute = Route + "/client-label"
const CoverageNote = "Best-effort execution attempts since installation; no historical backfill or replay guarantee. Empty buckets mean no observed events, not proof of no traffic; windows preceding installation or retention are partial. requests is unavailable: TraceID is optional and not unique. Token fidelity is reconstructed from seven ABI counters using SDK provider/executor semantics; original quality and presence are unavailable. Canonical input/output are not universally exclusive buckets. Zero-only, inconsistent and unclassified events are unknown. Cache rate uses reconstructed complete input totals only and is null if any event has unknown token/cache semantics. Missing execution IDs are dropped. Health counters are lifetime best-effort checkpoints."

type Config struct {
	PricingProviderMap StringMap `yaml:"pricing_provider_map"`
	PricingModelMap    StringMap `yaml:"pricing_model_map"`
	PricingEnabled     bool      `yaml:"pricing_enabled"`
	Directory          string    `yaml:"data_dir"`
	RetentionDays      int       `yaml:"retention_days"`
	MaxEvents          int       `yaml:"max_events"`
	MaxClients         int       `yaml:"max_clients"`
	MaxModels          int       `yaml:"max_models"`
}

func DefaultConfig(dir string) Config {
	return Config{Directory: dir, RetentionDays: 90, MaxEvents: 100000, MaxClients: 1000, MaxModels: 256, PricingEnabled: true}
}
func (c Config) Validate() error {
	if !c.validatePriceMaps() {
		return errors.New("invalid pricing mappings")
	}
	if !filepath.IsAbs(c.Directory) || filepath.Clean(c.Directory) == "/" {
		return errors.New("data_dir must be an absolute dedicated directory")
	}
	if c.RetentionDays < 1 || c.RetentionDays > 365 || c.MaxEvents < 1 || c.MaxEvents > 1000000 || c.MaxClients < 1 || c.MaxClients > 1000 || c.MaxModels < 1 || c.MaxModels > 256 {
		return errors.New("configuration limits out of bounds")
	}
	return nil
}

type Query struct{ Range, Client, Bucket string }

func (q Query) Window(now time.Time) (time.Time, time.Duration, error) {
	var d time.Duration
	switch q.Range {
	case "", "24h":
		d = 24 * time.Hour
	case "7d":
		d = 7 * 24 * time.Hour
	case "30d":
		d = 30 * 24 * time.Hour
	case "90d":
		d = 90 * 24 * time.Hour
	default:
		return time.Time{}, 0, errors.New("invalid range")
	}
	step := 24 * time.Hour
	if q.Bucket == "hour" || (q.Bucket == "" && d == 24*time.Hour) {
		step = time.Hour
	} else if q.Bucket != "" && q.Bucket != "day" {
		return time.Time{}, 0, errors.New("invalid bucket")
	}
	if !validClient(q.Client) {
		return time.Time{}, 0, errors.New("invalid client")
	}
	return now.Add(-d), step, nil
}

func validClient(id string) bool {
	if id == "" || id == "unattributed" {
		return true
	}
	for _, prefix := range []string{"native_", "plugin_"} {
		if strings.HasPrefix(id, prefix) {
			suffix := strings.TrimPrefix(id, prefix)
			_, err := hex.DecodeString(suffix)
			return err == nil && len(suffix) == 32 && strings.ToLower(suffix) == suffix
		}
	}
	return false
}

type Metrics struct {
	UnpricedReasons          map[string]int64 `json:"unpriced_reasons,omitempty"`
	PricedAttempts           int64            `json:"priced_attempts"`
	SavingsPricedAttempts    int64            `json:"savings_priced_attempts"`
	EstimatedCostUSD         *float64         `json:"estimated_cost_usd"`
	EstimatedCacheSavingsUSD *float64         `json:"estimated_cache_savings_usd"`
	Attempts                 int64            `json:"attempts"`
	Succeeded                int64            `json:"succeeded"`
	Failed                   int64            `json:"failed"`
	Requests                 *int64           `json:"requests"`
	InputTokens              int64            `json:"input_tokens"`
	OutputTokens             int64            `json:"output_tokens"`
	CacheReadTokens          int64            `json:"cache_read_tokens"`
	CacheWriteTokens         int64            `json:"cache_write_tokens"`
	UnclassifiedCachedTokens int64            `json:"unclassified_cached_tokens"`
	TotalTokens              int64            `json:"total_tokens"`
	CacheRate                *float64         `json:"cache_rate"`
	CacheRateDenominator     int64            `json:"cache_rate_denominator"`
	UnknownTokenEvents       int64            `json:"unknown_token_events"`
}

func (m *Metrics) add(v Metrics) {
	if len(v.UnpricedReasons) == 0 && v.Attempts > v.PricedAttempts {
		m.addUnpriced("legacy_unpriced", v.Attempts-v.PricedAttempts)
	} else {
		for reason, count := range v.UnpricedReasons {
			m.addUnpriced(reason, count)
		}
	}
	m.PricedAttempts += v.PricedAttempts
	m.SavingsPricedAttempts += v.SavingsPricedAttempts
	addOptional(&m.EstimatedCostUSD, v.EstimatedCostUSD)
	addOptional(&m.EstimatedCacheSavingsUSD, v.EstimatedCacheSavingsUSD)
	m.Attempts += v.Attempts
	m.Succeeded += v.Succeeded
	m.Failed += v.Failed
	m.InputTokens += v.InputTokens
	m.OutputTokens += v.OutputTokens
	m.CacheReadTokens += v.CacheReadTokens
	m.CacheWriteTokens += v.CacheWriteTokens
	m.UnclassifiedCachedTokens += v.UnclassifiedCachedTokens
	m.TotalTokens += v.TotalTokens
	m.CacheRateDenominator += v.CacheRateDenominator
	m.UnknownTokenEvents += v.UnknownTokenEvents
}
func (m *Metrics) addUnpriced(reason string, count int64) {
	if m.UnpricedReasons == nil {
		m.UnpricedReasons = map[string]int64{}
	}
	m.UnpricedReasons[reason] += count
}
func (m *Metrics) unpriced(reason string) {
	m.UnpricedReasons = map[string]int64{reason: 1}
}
func addOptional(dst **float64, src *float64) {
	if src != nil {
		if *dst == nil {
			*dst = new(float64)
		}
		**dst += *src
	}
}
func (m *Metrics) finish() {
	if m.UnknownTokenEvents == 0 && m.UnclassifiedCachedTokens == 0 && m.CacheRateDenominator > 0 {
		v := float64(m.CacheReadTokens) / float64(m.CacheRateDenominator)
		m.CacheRate = &v
	}
}

type Client struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	KeyHint   string `json:"key_hint"`
}
type ClientUsage struct {
	ClientID string   `json:"client"`
	Models   []string `json:"models"`
	Metrics
}
type Collection struct {
	State               string  `json:"state"`
	StartedAt           string  `json:"started_at"`
	EarliestEventAt     *string `json:"earliest_event_at"`
	RetentionDays       int     `json:"retention_days"`
	Delivery            string  `json:"delivery"`
	IngestionFailures   int64   `json:"ingestion_failures"`
	DroppedEvents       int64   `json:"dropped_events"`
	Duplicates          int64   `json:"duplicates"`
	MissingExecutionIDs int64   `json:"missing_execution_ids"`
	CoverageNote        string  `json:"coverage_note"`
}
type Window struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Bucket string `json:"bucket"`
}
type Point struct {
	Time string `json:"time"`
	Metrics
}
type Model struct {
	Model string `json:"model"`
	Metrics
}
type Pricing struct {
	UnpricedReasons          map[string]int64 `json:"unpriced_reasons,omitempty"`
	Source                   string           `json:"source"`
	Currency                 string           `json:"currency"`
	Status                   string           `json:"status"`
	SnapshotVersion          *string          `json:"snapshot_version"`
	FetchedAt                *string          `json:"fetched_at"`
	SnapshotVersions         []string         `json:"snapshot_versions"`
	PricedAttempts           int64            `json:"priced_attempts"`
	UnpricedAttempts         int64            `json:"unpriced_attempts"`
	SavingsPricedAttempts    int64            `json:"savings_priced_attempts"`
	EstimatedCostUSD         *float64         `json:"estimated_cost_usd"`
	EstimatedCacheSavingsUSD *float64         `json:"estimated_cache_savings_usd"`
	Available                bool             `json:"available"`
	Reason                   string           `json:"reason"`
}
type Response struct {
	SchemaVersion  int           `json:"schema_version"`
	Collection     Collection    `json:"collection"`
	Window         Window        `json:"window"`
	Clients        []Client      `json:"clients"`
	ClientUsage    []ClientUsage `json:"client_usage"`
	SelectedClient *string       `json:"selected_client"`
	Totals         Metrics       `json:"totals"`
	Timeline       []Point       `json:"timeline"`
	Models         []Model       `json:"models"`
	Pricing        Pricing       `json:"pricing"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
