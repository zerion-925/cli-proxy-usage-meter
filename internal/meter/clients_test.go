package meter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestKeyNamesHintsAndAggregates(t *testing.T) {
	s, now := openTest(t)
	keys := []string{"sk-shared-prefix-first-private-key", "sk-shared-prefix-second-private-key"}
	for i, key := range keys {
		ingest(t, s, event(string(rune('a'+i)), key, now.Add(-time.Minute)), now)
	}
	all := query(t, s, Query{}, now)
	if len(all.ClientUsage) != 2 || len(all.Clients) != 2 || all.Clients[0].ID == all.Clients[1].ID {
		t.Fatalf("identities merged: %+v", all)
	}
	var sum Metrics
	for _, row := range all.ClientUsage {
		sum.add(row.Metrics)
		if len(row.Models) != 1 || row.Models[0] != "model" {
			t.Fatalf("models: %+v", row)
		}
	}
	if sum.Attempts != all.Totals.Attempts || sum.TotalTokens != all.Totals.TotalTokens {
		t.Fatal("key totals do not reconcile")
	}
	client := all.Clients[0]
	if client.KeyHint != "sk-share••••" || client.Label != client.KeyHint {
		t.Fatalf("hint: %+v", client)
	}
	if err := s.SetClientName(context.Background(), client.ID, "  Team A  "); err != nil {
		t.Fatal(err)
	}
	r := event("c", keys[0], now.Add(-time.Second))
	ingest(t, s, r, now)
	named := query(t, s, Query{Client: client.ID}, now)
	for _, c := range named.Clients {
		if c.ID == client.ID && (c.Name != "Team A" || c.Label != "Team A") {
			t.Fatalf("name lost: %+v", c)
		}
	}
	raw, _ := json.Marshal(named)
	for _, key := range keys {
		if strings.Contains(string(raw), key) {
			t.Fatal("full API key exposed")
		}
	}
	if err := s.SetClientName(context.Background(), client.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientName(context.Background(), "native_00000000000000000000000000000000", "Unknown"); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	for _, name := range []string{strings.Repeat("a", 81), "bad\nname", "bad\u202ename"} {
		if err := s.SetClientName(context.Background(), client.ID, name); !errors.Is(err, ErrClientName) {
			t.Fatalf("invalid name accepted: %q", name)
		}
	}
}
func TestMaskedKeyNeverExposesShortOrUnsafeKeys(t *testing.T) {
	for _, key := range []string{"", "short", "12345678901", "👀long-private-key", "bad\nprivate-secret"} {
		if got := maskedKeyPrefix(key); got != "••••" {
			t.Fatalf("unsafe hint %q", got)
		}
	}
}
