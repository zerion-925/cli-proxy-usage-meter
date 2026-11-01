package meter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

const refreshInterval = 24 * time.Hour
const maxNormalizedBytes = 8 << 20

type catalogFetcher func(context.Context) ([]byte, error)

func (s *Store) loadPricing(ctx context.Context) error {
	var raw []byte
	var version, fetched string
	err := s.db.QueryRowContext(ctx, "SELECT version,fetched_at,catalog FROM price_catalog WHERE id=1").Scan(&version, &fetched, &raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var c catalog
		if len(raw) > maxNormalizedBytes || json.Unmarshal(raw, &c) != nil {
			return errCatalog
		}
		s.fetchedAt, err = time.Parse(time.RFC3339Nano, fetched)
		if err != nil {
			return errCatalog
		}
		c.Version = version
		s.catalog = &c
	}
	var failed bool
	err = s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='pricing_error'").Scan(&failed)
	s.pricingError.Store(failed)
	return err
}

// Called with lifecycle serialization, after the previous pricing worker joins.
func (s *Store) startPricing() {
	if s.fetch == nil || !s.cfg.PricingEnabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.pricingCancel = cancel
	s.pricingDone = done
	next := time.Until(s.fetchedAt.Add(refreshInterval))
	if s.pricingError.Load() || s.catalog == nil {
		next = 0
	}
	go func() {
		defer close(done)
		timer := time.NewTimer(max(0, next))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				_ = s.refreshPricing(ctx, s.fetch, time.Now().UTC())
				timer.Reset(refreshInterval)
			}
		}
	}()
}

func (s *Store) refreshPricing(ctx context.Context, fetch catalogFetcher, now time.Time) error {
	raw, err := fetch(ctx)
	var c *catalog
	var normalized []byte
	if err == nil {
		c, err = parseCatalog(raw)
	}
	if err == nil {
		normalized, err = json.Marshal(c)
		if len(normalized) > maxNormalizedBytes {
			err = errCatalog
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dbctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if !s.acquire(dbctx) {
		if ctx.Err() == nil {
			s.pricingError.Store(true)
		}
		return ErrStorage
	}
	defer s.release()
	if s.closed || ctx.Err() != nil {
		return ErrStorage
	}
	if err != nil {
		s.pricingError.Store(true)
		_, _ = s.db.ExecContext(dbctx, "UPDATE meta SET value='true' WHERE key='pricing_error'")
		return errCatalog
	}
	tx, e := s.db.BeginTx(dbctx, nil)
	if e != nil {
		s.pricingError.Store(true)
		return ErrStorage
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(dbctx, `INSERT OR IGNORE INTO price_versions VALUES(?,?,'models.dev','USD');
 INSERT INTO price_catalog VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version,fetched_at=excluded.fetched_at,catalog=excluded.catalog;
 DELETE FROM price_versions WHERE version<>? AND NOT EXISTS(SELECT 1 FROM events WHERE price_version=price_versions.version);
 UPDATE meta SET value='false' WHERE key='pricing_error';`, c.Version, stamp(now), c.Version, stamp(now), normalized, c.Version)
	if e != nil {
		s.pricingError.Store(true)
		return ErrStorage
	}
	if tx.Commit() != nil {
		s.pricingError.Store(true)
		return ErrStorage
	}
	s.catalog = c
	s.fetchedAt = now
	s.pricingError.Store(false)
	return nil
}
func (s *Store) pricingStatus(now time.Time) Pricing {
	p := Pricing{Source: "models.dev", Currency: "USD", Status: "loading", SnapshotVersions: []string{}}
	if s.catalog != nil {
		version := s.catalog.Version
		fetched := stamp(s.fetchedAt)
		p.SnapshotVersion = &version
		p.FetchedAt = &fetched
		p.Status = "ready"
		if s.pricingError.Load() || now.Sub(s.fetchedAt) > refreshInterval {
			p.Status = "stale"
		}
	} else if s.pricingError.Load() {
		p.Status = "error"
	}
	if !s.cfg.PricingEnabled {
		p.Status = "disabled"
	}
	return p
}
func (p *Pricing) finish(m Metrics, versions map[string]bool) {
	p.Available = m.PricedAttempts > 0
	p.PricedAttempts = m.PricedAttempts
	p.UnpricedAttempts = m.Attempts - m.PricedAttempts
	p.SavingsPricedAttempts = m.SavingsPricedAttempts
	p.EstimatedCostUSD = m.EstimatedCostUSD
	p.EstimatedCacheSavingsUSD = m.EstimatedCacheSavingsUSD
	for v := range versions {
		p.SnapshotVersions = append(p.SnapshotVersions, v)
	}
	sort.Strings(p.SnapshotVersions)
	p.Reason = "No priced attempts in this selection. Old history is never repriced."
	if p.Available {
		p.Reason = "Estimated standard text API list-price subtotal for priced attempts only; not a bill. OAuth/subscription usage is API-equivalent. Savings cover only attempts with comparable input/cache-read rates."
		if p.UnpricedAttempts > 0 {
			p.Reason = "Partial coverage. " + p.Reason
		}
	}
	switch p.Status {
	case "stale":
		p.Reason += " Cached catalog is stale or its refresh failed."
	case "error":
		p.Reason += " Catalog refresh failed."
	case "loading":
		p.Reason += " Catalog is loading."
	case "disabled":
		p.Reason += " Pricing is disabled for new attempts."
	}
}
