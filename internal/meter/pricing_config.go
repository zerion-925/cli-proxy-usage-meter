package meter

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// StringMap accepts YAML mappings and SDK UI string fields containing JSON objects.
type StringMap map[string]string

func (m *StringMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value == "" {
			*m = nil
			return nil
		}
		return json.Unmarshal([]byte(n.Value), m)
	}
	var plain map[string]string
	if err := n.Decode(&plain); err != nil {
		return err
	}
	*m = plain
	return nil
}
func validPriceID(v string) bool    { return v != "unknown" && safeModel(v, "") == v }
func validProviderID(v string) bool { return validPriceID(v) && !strings.ContainsAny(v, "/:") }
func (c Config) validatePriceMaps() bool {
	if len(c.PricingProviderMap) > 128 || len(c.PricingModelMap) > 256 {
		return false
	}
	for k, v := range c.PricingProviderMap {
		if !validProviderID(k) || !validProviderID(v) {
			return false
		}
	}
	for k, v := range c.PricingModelMap {
		p, m, ok := strings.Cut(k, "/")
		if !ok || !validProviderID(p) || !validPriceID(m) || !validPriceID(v) {
			return false
		}
	}
	return true
}
