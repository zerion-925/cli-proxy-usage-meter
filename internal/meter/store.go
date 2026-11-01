package meter

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var ErrStorage = errors.New("usage storage unavailable")

type Store struct {
	gate                                   chan struct{}
	db                                     *sql.DB
	lock                                   *os.File
	cfg                                    Config
	salt                                   []byte
	instance, started                      string
	closed                                 bool
	failures, dropped, duplicates, missing atomic.Int64
}

func Open(cfg Config) (_ *Store, err error) {
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cfg.Directory, 0700); err != nil {
		return nil, ErrStorage
	}
	info, e := os.Lstat(cfg.Directory)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("data_dir must be a real owner-only directory (0700)")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("data_dir must belong to the current user")
	}
	// Dedicated directories and no symlinks prevent accidental writes through an existing link.
	for _, n := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm", "writer.lock"} {
		p := filepath.Join(cfg.Directory, n)
		i, e := os.Lstat(p)
		if e == nil {
			if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 {
				return nil, errors.New("storage files must be regular owner-only files (0600)")
			}
			if st := i.Sys().(*syscall.Stat_t); st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
				return nil, ErrStorage
			}
		} else if !os.IsNotExist(e) {
			return nil, ErrStorage
		}
	}
	lock, e := os.OpenFile(filepath.Join(cfg.Directory, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, ErrStorage
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return nil, errors.New("data_dir is already in use")
	}
	s := &Store{cfg: cfg, lock: lock, gate: make(chan struct{}, 1)}
	defer func() {
		if err != nil {
			if s.db != nil {
				s.db.Close()
			}
			lock.Close()
		}
	}()
	file, e := os.OpenFile(filepath.Join(cfg.Directory, "usage.sqlite"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, ErrStorage
	}
	if e = file.Close(); e != nil {
		return nil, ErrStorage
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(cfg.Directory, "usage.sqlite")}
	u.RawQuery = "_journal_mode=WAL&_busy_timeout=100&_synchronous=NORMAL&_txlock=immediate&_secure_delete=on&_foreign_keys=on"
	s.db, err = sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, ErrStorage
	}
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var version int
	if err = s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, ErrStorage
	}
	if version > 2 {
		return nil, errors.New("database schema newer than this plugin")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, at INTEGER NOT NULL, client TEXT NOT NULL, namespace TEXT NOT NULL, model TEXT NOT NULL, metrics BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS events_time ON events(at);
 CREATE INDEX IF NOT EXISTS events_client_time ON events(client,at);
 CREATE INDEX IF NOT EXISTS events_model ON events(model);
 PRAGMA user_version=1;`)
	if e != nil {
		return nil, ErrStorage
	}
	if version < 2 {
		_, e = tx.ExecContext(ctx, `
        CREATE TABLE client_dims (id TEXT PRIMARY KEY, refs INTEGER NOT NULL);
        CREATE TABLE model_dims (id TEXT PRIMARY KEY, refs INTEGER NOT NULL);
        CREATE TABLE event_count (id INTEGER PRIMARY KEY CHECK(id=1), total INTEGER NOT NULL);
        INSERT INTO client_dims SELECT client,COUNT(*) FROM events GROUP BY client;
        INSERT INTO model_dims SELECT model,COUNT(*) FROM events GROUP BY model;
        INSERT INTO event_count SELECT 1,COUNT(*) FROM events;
        CREATE TRIGGER events_insert AFTER INSERT ON events BEGIN
          INSERT INTO client_dims VALUES(new.client,1) ON CONFLICT(id) DO UPDATE SET refs=refs+1;
          INSERT INTO model_dims VALUES(new.model,1) ON CONFLICT(id) DO UPDATE SET refs=refs+1;
          UPDATE event_count SET total=total+1 WHERE id=1;
        END;
        CREATE TRIGGER events_delete AFTER DELETE ON events BEGIN
          UPDATE client_dims SET refs=refs-1 WHERE id=old.client;
          DELETE FROM client_dims WHERE id=old.client AND refs=0;
          UPDATE model_dims SET refs=refs-1 WHERE id=old.model;
          DELETE FROM model_dims WHERE id=old.model AND refs=0;
          UPDATE event_count SET total=total-1 WHERE id=1;
        END;`)
		if e != nil {
			return nil, ErrStorage
		}
	}
	if _, e = tx.ExecContext(ctx, "PRAGMA user_version=2"); e != nil {
		return nil, ErrStorage
	}
	salt := make([]byte, 32)
	if _, e = rand.Read(salt); e != nil {
		return nil, ErrStorage
	}
	instance := make([]byte, 16)
	if _, e = rand.Read(instance); e != nil {
		return nil, ErrStorage
	}
	for k, v := range map[string]any{"salt": salt, "instance": hex.EncodeToString(instance), "started": stamp(time.Now()), "health": "[0,0,0,0]"} {
		if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO meta VALUES (?,?)", k, v); e != nil {
			return nil, ErrStorage
		}
	}
	if e = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='salt'").Scan(&s.salt); e != nil {
		return nil, ErrStorage
	}
	if e = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='instance'").Scan(&s.instance); e != nil {
		return nil, ErrStorage
	}
	if e = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='started'").Scan(&s.started); e != nil {
		return nil, ErrStorage
	}
	var health []byte
	var values [4]int64
	if e = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='health'").Scan(&health); e != nil {
		return nil, ErrStorage
	}
	if json.Unmarshal(health, &values) != nil || len(s.salt) != 32 || len(s.instance) != 32 {
		return nil, ErrStorage
	}
	s.failures.Store(values[0])
	s.dropped.Store(values[1])
	s.duplicates.Store(values[2])
	s.missing.Store(values[3])
	if e = tx.Commit(); e != nil {
		return nil, ErrStorage
	}
	if _, e = s.db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=256; PRAGMA journal_size_limit=4194304; PRAGMA max_page_count=262144"); e != nil {
		return nil, ErrStorage
	}
	return s, nil
}
func (s *Store) opaque(parts ...string) string {
	h := hmac.New(sha256.New, s.salt)
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
func (s *Store) Drop(missing bool) {
	s.dropped.Add(1)
	if missing {
		s.missing.Add(1)
	}
}
func (s *Store) Failure()            { s.failures.Add(1); s.Drop(false) }
func (s *Store) MaintenanceFailure() { s.failures.Add(1) }
func (s *Store) health(ctx context.Context, tx *sql.Tx) error {
	b, _ := json.Marshal([4]int64{s.failures.Load(), s.dropped.Load(), s.duplicates.Load(), s.missing.Load()})
	_, e := tx.ExecContext(ctx, "UPDATE meta SET value=? WHERE key='health'", b)
	return e
}
func (s *Store) acquire(ctx context.Context) bool {
	select {
	case s.gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (s *Store) release() { <-s.gate }
func (s *Store) Update(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !s.acquire(ctx) {
		return ErrStorage
	}
	defer s.release()
	if s.closed {
		return ErrStorage
	}
	if cfg.Directory != s.cfg.Directory {
		return errors.New("data_dir changes require restart")
	}
	s.cfg = cfg
	return nil
}
func (s *Store) Ingest(ctx context.Context, r pluginapi.UsageRecord, now time.Time) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if !s.acquire(ctx) {
		s.Failure()
		return ErrStorage
	}
	defer s.release()
	if s.closed {
		s.Failure()
		return ErrStorage
	}
	accounted := false
	cutoff := now.AddDate(0, 0, -s.cfg.RetentionDays).UnixNano()
	skip := false
	if strings.TrimSpace(r.RequestID) == "" {
		s.Drop(true)
		skip = true
		accounted = true
	} else if !validRecord(r, now, cutoff) {
		s.Drop(false)
		skip = true
		accounted = true
	}
	defer func() {
		if err != nil {
			s.failures.Add(1)
			if !accounted {
				s.Drop(false)
			}
			err = ErrStorage
		}
	}()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "DELETE FROM events WHERE id IN (SELECT id FROM events WHERE at < ? ORDER BY at LIMIT 1000)", cutoff); e != nil {
		return e
	}
	if !skip {
		id := s.opaque("execution", s.instance, r.RequestID)
		var exists int
		e = tx.QueryRowContext(ctx, "SELECT 1 FROM events WHERE id=?", id).Scan(&exists)
		if e == nil {
			s.duplicates.Add(1)
			accounted = true
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		} else {
			ns := "unattributed"
			client := "unattributed"
			if r.APIKey != "" && (r.IsNativeKey || r.AccessProvider != "") {
				if r.IsNativeKey {
					ns = "native"
				} else {
					ns = "plugin"
				}
				client = ns + "_" + s.opaque("client", ns, r.AccessProvider, r.APIKey)
			}
			model := safeModel(r.Model, r.APIKey)
			var count, clients, models int
			if e = tx.QueryRowContext(ctx, `SELECT (SELECT total FROM event_count WHERE id=1), (SELECT COUNT(*) FROM client_dims), (SELECT COUNT(*) FROM model_dims)`).Scan(&count, &clients, &models); e != nil {
				return e
			}
			var hasClient, hasModel bool
			if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_dims WHERE id=?), EXISTS(SELECT 1 FROM model_dims WHERE id=?)", client, model).Scan(&hasClient, &hasModel); e != nil {
				return e
			}
			if count >= s.cfg.MaxEvents || (!hasClient && clients >= s.cfg.MaxClients) || (!hasModel && models >= s.cfg.MaxModels) {
				s.Drop(false)
				accounted = true
			} else {
				b, _ := json.Marshal(tokenMetrics(r))
				if _, e = tx.ExecContext(ctx, "INSERT INTO events VALUES(?,?,?,?,?,?)", id, r.RequestedAt.UnixNano(), client, ns, model, b); e != nil {
					return e
				}
			}
		}
	}
	if e = s.health(ctx, tx); e != nil {
		return e
	}
	return tx.Commit()
}
func validRecord(r pluginapi.UsageRecord, now time.Time, cutoff int64) bool {
	if len(r.RequestID) > 256 || len(r.APIKey) > 4096 || len(r.AccessProvider) > 256 || len(r.Provider) > 128 || len(r.ExecutorType) > 128 || r.RequestedAt.IsZero() || r.RequestedAt.Before(time.Unix(0, cutoff)) || r.RequestedAt.After(now.Add(time.Minute)) {
		return false
	}
	d := r.Detail
	for _, v := range []int64{d.InputTokens, d.OutputTokens, d.ReasoningTokens, d.CachedTokens, d.CacheReadTokens, d.CacheCreationTokens, d.TotalTokens} {
		if v < 0 || v > 1000000000 {
			return false
		}
	}
	return true
}
func safeModel(v, key string) string {
	if v == "" || len(v) > 128 || (key != "" && strings.Contains(v, key)) || strings.Contains(strings.ToLower(v), "sk-") {
		return "unknown"
	}
	for _, c := range v {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("-_.:/", c)) {
			return "unknown"
		}
	}
	return v
}
func tokenMetrics(r pluginapi.UsageRecord) Metrics {
	d := r.Detail
	reconstructed := usage.EnsureTokenBreakdownForProvider(usage.Detail{InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, ReasoningTokens: d.ReasoningTokens, CachedTokens: d.CachedTokens, CacheReadTokens: d.CacheReadTokens, CacheCreationTokens: d.CacheCreationTokens, TotalTokens: d.TotalTokens}, r.Provider, r.ExecutorType)
	b := reconstructed.TokenBreakdown
	m := Metrics{Attempts: 1, InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CacheReadTokens: d.CacheReadTokens, CacheWriteTokens: d.CacheCreationTokens, TotalTokens: b.TotalTokens}
	if r.Failed {
		m.Failed = 1
	} else {
		m.Succeeded = 1
	}
	m.UnclassifiedCachedTokens = max(0, d.CachedTokens-d.CacheReadTokens-d.CacheCreationTokens)
	if b.Quality != usage.TokenAccountingQualityComplete || b.TotalTokens == 0 || m.UnclassifiedCachedTokens > 0 {
		m.UnknownTokenEvents = 1
	} else {
		m.CacheRateDenominator = b.Input.TotalTokens
	}
	return m
}
func (s *Store) Query(ctx context.Context, q Query, now time.Time) (Response, error) {
	from, step, e := q.Window(now)
	if e != nil {
		return Response{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if !s.acquire(ctx) {
		return Response{}, ErrStorage
	}
	defer s.release()
	if s.closed {
		return Response{}, ErrStorage
	}
	cutoff := now.AddDate(0, 0, -s.cfg.RetentionDays)
	resp := Response{SchemaVersion: 1, Collection: Collection{State: "collecting", StartedAt: s.started, RetentionDays: s.cfg.RetentionDays, Delivery: "best_effort", CoverageNote: CoverageNote, IngestionFailures: s.failures.Load(), DroppedEvents: s.dropped.Load(), Duplicates: s.duplicates.Load(), MissingExecutionIDs: s.missing.Load()}, Window: Window{From: stamp(from), To: stamp(now), Bucket: "day"}, Clients: []Client{}, Timeline: []Point{}, Models: []Model{}, Pricing: Pricing{Reason: "No price configuration; cost is unavailable."}}
	if step == time.Hour {
		resp.Window.Bucket = "hour"
	}
	if q.Client != "" {
		resp.SelectedClient = &q.Client
	}
	rows, e := s.db.QueryContext(ctx, "SELECT client,namespace,MIN(at) FROM events WHERE at>=? AND at<? GROUP BY client,namespace ORDER BY client LIMIT 1001", cutoff.UnixNano(), now.UnixNano())
	if e != nil {
		return Response{}, ErrStorage
	}
	earliest := now
	for rows.Next() {
		var c Client
		var at int64
		if e = rows.Scan(&c.ID, &c.Namespace, &at); e != nil {
			rows.Close()
			return Response{}, ErrStorage
		}
		c.Label = c.Namespace
		if c.Namespace != "unattributed" {
			c.Label += " " + c.ID[len(c.ID)-8:]
		}
		resp.Clients = append(resp.Clients, c)
		if t := time.Unix(0, at); t.Before(earliest) {
			earliest = t
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return Response{}, ErrStorage
	}
	if earliest.Before(now) {
		v := stamp(earliest)
		resp.Collection.EarliestEventAt = &v
	}
	bins := map[int64]*Metrics{}
	models := map[string]*Metrics{}
	rows, e = s.db.QueryContext(ctx, "SELECT at,model,metrics FROM events WHERE at>=? AND at<? AND (?='' OR client=?) ORDER BY at LIMIT ?", max(from.UnixNano(), cutoff.UnixNano()), now.UnixNano(), q.Client, q.Client, 1000001)
	if e != nil {
		return Response{}, ErrStorage
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 1000000 {
			return Response{}, ErrStorage
		}
		var at int64
		var model string
		var b []byte
		var m Metrics
		if e = rows.Scan(&at, &model, &b); e != nil {
			return Response{}, ErrStorage
		}
		if json.Unmarshal(b, &m) != nil {
			return Response{}, ErrStorage
		}
		resp.Totals.add(m)
		t := time.Unix(0, at).UTC().Truncate(step).UnixNano()
		if bins[t] == nil {
			bins[t] = &Metrics{}
		}
		bins[t].add(m)
		if models[model] == nil {
			models[model] = &Metrics{}
		}
		models[model].add(m)
	}
	if rows.Err() != nil {
		return Response{}, ErrStorage
	}
	for t := from.UTC().Truncate(step); t.Before(now); t = t.Add(step) {
		m := bins[t.UnixNano()]
		if m == nil {
			m = &Metrics{}
		}
		m.finish()
		resp.Timeline = append(resp.Timeline, Point{stamp(t), *m})
	}
	for name, m := range models {
		m.finish()
		resp.Models = append(resp.Models, Model{name, *m})
	}
	sort.Slice(resp.Models, func(i, j int) bool { return resp.Models[i].Model < resp.Models[j].Model })
	resp.Totals.finish()
	started, parseErr := time.Parse(time.RFC3339Nano, s.started)
	if parseErr != nil || from.Before(started) || from.Before(cutoff) || resp.Collection.DroppedEvents > 0 || resp.Collection.IngestionFailures > 0 || resp.Totals.UnknownTokenEvents > 0 {
		resp.Collection.State = "partial"
	}
	return resp, nil
}

// Maintain expires bounded batches even while there is no traffic, then checkpoints health.
func (s *Store) Maintain(now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if !s.acquire(ctx) {
		return ErrStorage
	}
	defer s.release()
	if s.closed {
		return ErrStorage
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "DELETE FROM events WHERE id IN (SELECT id FROM events WHERE at<? ORDER BY at LIMIT 1000)", now.AddDate(0, 0, -s.cfg.RetentionDays).UnixNano()); e != nil {
		return ErrStorage
	}
	if e = s.health(ctx, tx); e != nil {
		return ErrStorage
	}
	if tx.Commit() != nil {
		return ErrStorage
	}
	if _, e = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !s.acquire(ctx) {
		return ErrStorage
	}
	defer s.release()
	if s.closed {
		return nil
	}
	s.closed = true
	tx, e := s.db.BeginTx(ctx, nil)
	if e == nil {
		e = s.health(ctx, tx)
		if e == nil {
			e = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	return errors.Join(e, s.db.Close(), s.lock.Close())
}
