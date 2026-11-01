package meter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var errCatalog = errors.New("pricing catalog unavailable or invalid")

type priceRates struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	Reasoning  *float64 `json:"reasoning,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}
type priceTier struct {
	Size  int64      `json:"size"`
	Rates priceRates `json:"rates"`
}
type priceModel struct {
	Rates       priceRates  `json:"rates"`
	Tiers       []priceTier `json:"tiers,omitempty"`
	Unsupported bool        `json:"unsupported,omitempty"`
}
type catalog struct {
	Version string                           `json:"-"`
	Models  map[string]map[string]priceModel `json:"models"`
}

func parseCatalog(raw []byte) (*catalog, error) {
	if !strictCatalogJSON(raw) {
		return nil, errCatalog
	}
	var providers map[string]struct {
		Models map[string]struct {
			Cost       json.RawMessage `json:"cost"`
			Modalities struct {
				Output []string `json:"output"`
			} `json:"modalities"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &providers) != nil || len(providers) == 0 || len(providers) > 512 {
		return nil, errCatalog
	}
	h := sha256.Sum256(raw)
	c := &catalog{Version: hex.EncodeToString(h[:]), Models: map[string]map[string]priceModel{}}
	total, kept := 0, 0
	for p, v := range providers {
		total += len(v.Models)
		if total > 20000 {
			return nil, errCatalog
		}
		if !validProviderID(p) {
			continue
		}
		c.Models[p] = map[string]priceModel{}
		for m, v := range v.Models {
			if !validPriceID(m) {
				continue
			}
			model, err := parsePriceModel(v.Cost)
			if err != nil {
				return nil, err
			}
			for _, mode := range v.Modalities.Output {
				if mode != "text" {
					model.Unsupported = true
				}
			}
			kept++
			c.Models[p][m] = model
		}
	}
	if kept == 0 {
		return nil, errCatalog
	}
	return c, nil
}
func (c *catalog) estimate(cfg Config, r pluginapi.UsageRecord, m *Metrics) {
	if m.UnknownTokenEvents != 0 || c == nil {
		return
	}
	// The response is authoritative; then final outbound tier, then requested tier.
	tier := r.ResponseServiceTier
	if tier == "" {
		tier = r.EffectiveServiceTier
	}
	if tier == "" {
		tier = r.ServiceTier
	}
	if tier != "" && tier != "default" && tier != "standard" {
		return
	}
	provider := map[string]string{"openai": "openai", "codex": "openai", "claude": "anthropic", "gemini": "google"}[r.Provider]
	if mapped, ok := cfg.PricingProviderMap[r.Provider]; ok {
		provider = mapped
	}
	if provider == "" {
		return
	}
	name := r.Model
	if mapped, ok := cfg.PricingModelMap[r.Provider+"/"+name]; ok {
		name = mapped
	}
	model, ok := c.Models[provider][name]
	if !ok || model.Unsupported {
		return
	}
	b := tokenBreakdown(r)
	if b.Input.CacheWriteTokens > 0 {
		return
	} // The ABI carries no cache-write TTL.
	rates := model.Rates
	for _, tier := range model.Tiers {
		if b.Input.TotalTokens > tier.Size {
			rates = tier.Rates
		}
	}
	// models.dev's output rate includes reasoning unless a separate rate is published.
	reasoning := rates.Reasoning
	if reasoning == nil {
		reasoning = rates.Output
	}
	cost := 0.0
	for _, part := range []struct {
		n    int64
		rate *float64
	}{{b.Input.UncachedTokens, rates.Input}, {b.Input.CacheReadTokens, rates.CacheRead}, {b.Input.CacheWriteTokens, rates.CacheWrite}, {b.Output.NonReasoningTokens, rates.Output}, {b.Output.ReasoningTokens, reasoning}} {
		if part.n > 0 {
			if part.rate == nil {
				return
			}
			cost += float64(part.n) * (*part.rate) / 1e6
		}
	}
	m.PricedAttempts = 1
	m.EstimatedCostUSD = &cost
	if rates.Input != nil && rates.CacheRead != nil {
		savings := float64(b.Input.CacheReadTokens) * (*rates.Input - *rates.CacheRead) / 1e6
		m.SavingsPricedAttempts = 1
		m.EstimatedCacheSavingsUSD = &savings
	}
}

func parsePriceModel(raw []byte) (priceModel, error) {
	m := priceModel{}
	if len(raw) == 0 || string(raw) == "null" {
		return m, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &m.Rates) != nil {
		return m, errCatalog
	}
	for _, rate := range []*float64{m.Rates.Input, m.Rates.Output, m.Rates.Reasoning, m.Rates.CacheRead, m.Rates.CacheWrite} {
		if rate != nil && (math.IsNaN(*rate) || math.IsInf(*rate, 0) || *rate < 0 || *rate > 1e9) {
			return m, errCatalog
		}
	}
	for k := range fields {
		switch k {
		case "input", "output", "reasoning", "cache_read", "cache_write", "tiers", "context_over_200k":
		default:
			m.Unsupported = true
		}
	}
	if t, ok := fields["tiers"]; ok {
		var tiers []struct {
			Tier struct {
				Type string `json:"type"`
				Size int64  `json:"size"`
			} `json:"tier"`
		}
		var raws []json.RawMessage
		if json.Unmarshal(t, &tiers) != nil || json.Unmarshal(t, &raws) != nil || len(tiers) > 16 {
			return m, errCatalog
		}
		for i, tier := range tiers {
			var f map[string]json.RawMessage
			json.Unmarshal(raws[i], &f)
			var descriptor map[string]json.RawMessage
			if json.Unmarshal(f["tier"], &descriptor) != nil {
				return m, errCatalog
			}
			for key := range descriptor {
				if key != "type" && key != "size" {
					m.Unsupported = true
				}
			}
			delete(f, "tier")
			b, _ := json.Marshal(f)
			child, err := parsePriceModel(b)
			if err != nil {
				return m, err
			}
			if tier.Tier.Type != "context" || tier.Tier.Size <= 0 || tier.Tier.Size > 1000000000 || child.Unsupported || len(child.Tiers) > 0 {
				m.Unsupported = true
			}
			m.Tiers = append(m.Tiers, priceTier{tier.Tier.Size, child.Rates})
		}
	} else if legacy, ok := fields["context_over_200k"]; ok {
		child, err := parsePriceModel(legacy)
		if err != nil {
			return m, err
		}
		if child.Unsupported || len(child.Tiers) > 0 {
			m.Unsupported = true
		}
		m.Tiers = []priceTier{{200000, child.Rates}}
	}
	sort.Slice(m.Tiers, func(i, j int) bool { return m.Tiers[i].Size < m.Tiers[j].Size })
	for i := 1; i < len(m.Tiers); i++ {
		if m.Tiers[i-1].Size == m.Tiers[i].Size {
			m.Unsupported = true
		}
	}
	return m, nil
}
