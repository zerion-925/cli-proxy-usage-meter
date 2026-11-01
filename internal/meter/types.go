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
const CoverageNote = "Best-effort execution attempts since installation; no historical backfill or replay guarantee. Empty buckets mean no observed events, not proof of no traffic; windows preceding installation or retention are partial. requests is unavailable: TraceID is optional and not unique. Token fidelity is reconstructed from seven ABI counters using SDK provider/executor semantics; original quality and presence are unavailable. Canonical input/output are not universally exclusive buckets. Zero-only, inconsistent and unclassified events are unknown. Cache rate uses reconstructed complete input totals only and is null if any event has unknown token/cache semantics. Missing execution IDs are dropped. Health counters are lifetime best-effort checkpoints."

type Config struct {
	Directory     string `yaml:"data_dir"`
	RetentionDays int    `yaml:"retention_days"`
	MaxEvents     int    `yaml:"max_events"`
	MaxClients    int    `yaml:"max_clients"`
	MaxModels     int    `yaml:"max_models"`
}

func DefaultConfig(dir string) Config { return Config{dir, 90, 100000, 1000, 256} }
func (c Config) Validate() error {
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
	Attempts                 int64    `json:"attempts"`
	Succeeded                int64    `json:"succeeded"`
	Failed                   int64    `json:"failed"`
	Requests                 *int64   `json:"requests"`
	InputTokens              int64    `json:"input_tokens"`
	OutputTokens             int64    `json:"output_tokens"`
	CacheReadTokens          int64    `json:"cache_read_tokens"`
	CacheWriteTokens         int64    `json:"cache_write_tokens"`
	UnclassifiedCachedTokens int64    `json:"unclassified_cached_tokens"`
	TotalTokens              int64    `json:"total_tokens"`
	CacheRate                *float64 `json:"cache_rate"`
	CacheRateDenominator     int64    `json:"cache_rate_denominator"`
	UnknownTokenEvents       int64    `json:"unknown_token_events"`
}

func (m *Metrics) add(v Metrics) {
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
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}
type Response struct {
	SchemaVersion  int        `json:"schema_version"`
	Collection     Collection `json:"collection"`
	Window         Window     `json:"window"`
	Clients        []Client   `json:"clients"`
	SelectedClient *string    `json:"selected_client"`
	Totals         Metrics    `json:"totals"`
	Timeline       []Point    `json:"timeline"`
	Models         []Model    `json:"models"`
	Pricing        Pricing    `json:"pricing"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
