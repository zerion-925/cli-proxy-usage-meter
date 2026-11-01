package meter

import (
	"bytes"
	"encoding/json"
	"io"
)

// Reject duplicate object keys (including identities/rates), excessive nesting,
// trailing JSON and enormous dimensions before decoding the whitelist.
func strictCatalogJSON(raw []byte) bool {
	if len(raw) > maxCatalogBytes {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	tokens := 0
	var value func(int) bool
	value = func(depth int) bool {
		tokens++
		if depth > 32 || tokens > 2000000 {
			return false
		}
		tok, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return false
				}
				seen[k] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
