# Native client usage meter

A standalone Go C-shared usage observer for CLIProxyAPI v8. It persists **execution attempts**, token counters and opaque client identities in SQLite, then serves the agreed frontend JSON contract at:

```text
GET /v0/management/client-usage-meter
```

There is no frontend, companion service, public data resource, or destructive usage-queue reader. The official host registers the plugin as an independent `RegisterNamedPlugin` usage sink. Other observers continue to receive their own callbacks.

## Build and verify

The module uses the **read-only** sibling checkout `../CLIProxyAPI-upstream-audit` through a local `replace`. Tested revision: `250d598175db61a4e99ac38b9164ed04745754e5`. Keep that checkout pinned: current SDK source, plugin ABI and provider normalization are part of this plugin's compatibility contract.

Requirements: Linux, Go 1.27.1, a working C compiler, CGO, and a local durable filesystem with SQLite locking/WAL support. The verified artifact is Linux ARM64. Other Unix targets have not been verified; Windows is unsupported because directory exclusivity uses `flock`.

```bash
cd /home/ubuntu/workspace/cli-proxy-usage-meter
pwd
make verify
```

This runs tests, race tests, vet, the native build, then the official-host integration test with `GOMAXPROCS=2` and `-p 2`. The actual artifacts are `build/client-usage-meter.so` and its generated C header. They are ignored by Git. To build only:

```bash
GOMAXPROCS=2 CGO_ENABLED=1 go build -p 2 -buildmode=c-shared \
  -o build/client-usage-meter.so ./cmd/usage-meter
```

`make runtime-race` runs a race-instrumented official host with the ordinary native library. Plugin logic is separately race-tested in-process by `make race`. Loading a race-instrumented shared library failed on this ARM64 environment with a ThreadSanitizer shadow-memory allocation error; native library instrumentation is not claimed.

The plugin exports `cliproxy_plugin_init`, `cliproxyPluginCall`, `cliproxyPluginFree`, and `cliproxyPluginShutdown`. JSON methods are `plugin.register`, `plugin.reconfigure`, `plugin.quiesce`, `plugin.shutdown`, `usage.handle`, `management.register`, and `management.handle`. Buffers are allocated/freed on the plugin side following the official C ABI examples; no Go pointers cross the boundary.

## Configure and run

Copy the `.so` into the host's dedicated plugin directory. The filename determines the plugin ID, so keep `client-usage-meter.so` and use `client-usage-meter` in `plugins.configs`. Example setup, as the same OS user that runs the proxy:

```bash
install -d -m 700 /absolute/path/to/plugins
install -d -m 700 /absolute/durable/path/to/usage-meter
install -m 700 build/client-usage-meter.so /absolute/path/to/plugins/client-usage-meter.so
```

Add this to the host configuration (v8 layout):

```yaml
management:
  allow-remote: false
  secret-key: "REPLACE_WITH_YOUR_MANAGEMENT_SECRET"
  disable-control-panel: true
plugins:
  enabled: true
  dir: /absolute/path/to/plugins
  configs:
    client-usage-meter:
      enabled: true
      data_dir: /absolute/durable/path/to/usage-meter
      retention_days: 90
      max_events: 100000
      max_clients: 1000
      max_models: 256
```

Use the normal, CGO-enabled official proxy binary with `--config /absolute/path/to/config.yaml`. Keep existing inference credentials and other settings. `data_dir` can instead come from the **only plugin environment variable**, `CLI_PROXY_USAGE_METER_DIR`; explicit YAML overrides it. There is deliberately no temporary-directory or current-directory fallback. The host's `MANAGEMENT_PASSWORD` is also supported by its existing authentication middleware; it is not a plugin-owned variable. No price configuration exists.

At the pinned revision, management authentication requires a configured secret hash or `MANAGEMENT_PASSWORD`, **even when the SDK's local management password option is set**. A missing management configuration returns 404. Home mode blocks in-process management (404 through its availability gate; its heartbeat gate can return 503 earlier). Plugin collection and management availability are separate concerns. Core `usage-statistics-enabled: false` does not disable this independent sink, as verified at runtime.

The exact `/v0/management` path is the approved plugin-host exception, not a fallback for core management APIs. Registration contains one GET route, an empty `Menu`, and **no `Resources`**. Do not add a Menu: the host turns it into an unauthenticated resource. The SDK requires a nonempty `GitHubRepository` metadata field; this unpublished local build explicitly uses `local-unpublished:cli-proxy-usage-meter`, not a fictitious hosted repository.

## Read API

```bash
curl --fail --silent --show-error \
  -H "Authorization: Bearer $METER_MANAGEMENT_KEY" \
  'http://127.0.0.1:8317/v0/management/client-usage-meter?range=24h'
```

The secret goes in the header, never a URL. `METER_MANAGEMENT_KEY` above is a shell variable for curl, not plugin configuration.

| Parameter | Accepted values |
| --- | --- |
| `range` | `24h` (default), `7d`, `30d`, `90d` |
| `bucket` | `hour` or `day`; defaults to hour for 24h and day otherwise |
| `client` | A returned `native_`/`plugin_` + 32 lowercase hex ID, or `unattributed`; omit for all clients |

Unknown parameters, duplicate parameters, invalid values, and GET bodies return 400. Other paths/methods are not registered (host 404; direct ABI method mismatch returns 405). Storage unavailable or query deadline exceeded returns 503, with no partial totals. Responses are `application/json` with `Cache-Control: no-store`. Parser and database errors are never echoed.

Response schema version 1 matches `../usage-meter-contract.md`: `collection`, `window`, `clients`, `selected_client`, `totals`, `timeline`, `models`, and `pricing`. Arrays remain arrays when empty. `requests` is always JSON null at every aggregation level. Unknown client IDs give empty metrics; the client list includes all retained identities, independent of the selected window/filter. Removing a key from the host does not erase its retained history.

Windows are rolling UTC intervals **[from, to)**. UTC hour/day buckets align to clock boundaries, so the first and last buckets may be partial. Empty buckets mean no observed events, not proof of no traffic. Windows extending before installation or retention report partial coverage. Empty buckets are emitted; 90 days with hourly buckets has at most 2161 points. `window.from` describes the requested range even if retention/installation covers less; `collection.started_at` and `earliest_event_at` communicate coverage. `earliest_event_at` is global retained history, not the selected client's minimum.

## Metric semantics

* **Attempts:** one retained execution for each persisted instance + canonical `RequestID`, hashed with the local HMAC secret. A retry with a new execution ID is another attempt. Re-delivery of the same ID is a duplicate, even after restart or with a changed key/model. Dedupe lasts while that event is retained; there is no permanent tombstone archive.
* **Requests:** always unavailable. `TraceID` is optional and has no sound unique inbound-request guarantee. The plugin neither persists it nor invents request counts from it.
* **Missing IDs:** drop the callback, increment `missing_execution_ids` and `dropped_events`, and never fabricate an ID from timestamps or payloads. The official usage publisher normally supplies an execution ID; direct malformed ABI callbacks can lack it.
* **Success/failure:** the host's execution `Failed` flag, not end-to-end application delivery or HTTP-request success. Attempts = succeeded + failed.
* **Input/output:** the canonical ABI counters are preserved. They are not universally uncached input or reasoning-exclusive output. Do not add these columns to cache/reasoning columns.
* **Total:** reconstructed via exported `usage.EnsureTokenBreakdownForProvider(detail, provider, executorType)` from the pinned SDK. This prevents cache/reasoning double counting. The ABI loses the original v2 breakdown, quality, and field presence. The plugin labels this **reconstructed fidelity** in `coverage_note`, never original complete fidelity.
* **Unknown:** zero-only, inconsistent, unknown-provider, or ambiguous aggregate-cache events increment `unknown_token_events`. A numeric zero is not proof that a measurement was available. For unknown providers the SDK preserves an authoritative total, or a conservative lower bound when total is absent; that lower bound is also marked unknown.
* **Cache:** read/write are explicit ABI counters. `unclassified_cached_tokens = max(0, CachedTokens - CacheReadTokens - CacheCreationTokens)`; this is an ambiguity indicator, not an extra bucket to add to total.
* **Cache denominator:** sum of SDK reconstructed `TokenBreakdown.Input.TotalTokens` **only for events with complete reconstruction, positive total, and no unclassified cached remainder**. For OpenAI/subset providers this is canonical input (cache included); for Claude/independent semantics it is input + cache reads + cache writes; for Gemini/separate-reasoning semantics it is canonical input (cache included). Provider/executor matching is the SDK's matching, including `OpenAICompatExecutor` for custom providers.
* **Cache rate:** explicit cache reads / the above denominator, expressed as a fraction 0–1. It is null when the denominator is zero or any event in that aggregate has unknown token/cache semantics. Denominator still reports the eligible subset when rate is null. Empty and failed zero-only usage therefore cannot imply a known 0% hit rate.
* **Price:** `pricing.available=false` with a reason. No fake zero cost, rates, currency, or historical price assumptions.

The SDK treats reasoning as a subset of output for OpenAI-like providers, separate from output for Gemini-like providers, and independent for Claude-like providers. Actual original provider quality cannot be recovered from seven integers; malformed/incomplete source counters remain uncertain even if a plausible reconstruction is possible.

## Persistence, limits and security

SQLite uses the maintained CGO driver [`mattn/go-sqlite3` v1.14.52](https://github.com/mattn/go-sqlite3/releases/tag/v1.14.52), bundled SQLite, WAL, `synchronous=NORMAL`, a 100 ms busy timeout, one connection, immediate write transactions, secure delete, indexed time/client lookups, and versioned transactional migrations. Version 1 introduced events/metadata; version 2 adds trigger-maintained row/dimension counts so ingestion never recounts the event table. Newer unknown schema versions fail closed.

The directory must be absolute, owned by the running UID, a real directory, and mode 0700. Database, lock, WAL and SHM files must be regular, owned by the UID, and mode 0600; existing symlinks/hardlinks or broad permissions are rejected. New files use owner-only modes without changing the proxy process's umask. SQLite inherits the database permissions for its WAL/SHM files, verified with live storage tests. Use a dedicated trusted local directory and persistent volume, never a shared/untrusted or ephemeral mount. A nonblocking `flock` allows one plugin writer per directory; multiple processes/instances require separate directories.

A random 32-byte HMAC secret and random persisted instance identity live in the database. Client IDs use domain-separated, length-framed HMAC-SHA256 truncated to 128 bits over namespace + AccessProvider + APIKey. Labels contain only namespace and a short opaque suffix. Native/plugin providers and different plugin auth providers stay separate even if their key string is identical. Missing key or missing nonnative attribution is grouped as `unattributed`. Deleting/resetting the database changes identity; copying it to another independent deployment also copies its namespace, so provision separate databases for separate hosts.

Persisted event fields are only hashed execution ID, timestamp, opaque client ID, namespace, bounded model identifier, and whitelisted aggregate metrics. Keys/provider identifiers exist briefly in callback memory for hashing. No raw keys, request IDs, headers, prompts, completions, bodies, failure text, credential/session/trace IDs, upstream URLs or pricing are persisted or logged by this plugin. The model label accepts only a bounded identifier alphabet; invalid/secret-looking names or names containing the current key become `unknown`. Arbitrary model identifiers remain operational metadata, not a content-scrubbing guarantee. Raw keys are rejected as client filters. Host logging and other plugins remain the host operator's responsibility.

| Bound | Default / hard maximum |
| --- | --- |
| Retention | 90 days / configurable 1–365 |
| Retained events | 100,000 / configurable 1–1,000,000 |
| Retained clients | 1,000 / configurable 1–1,000 |
| Retained models | 256 / configurable 1–256 |
| Callback queue | 256 records, nonblocking enqueue |
| ABI input | 1 MiB before copying into Go |
| Plugin YAML | 16 KiB decoded |
| Key / execution ID / provider / model | 4096 / 256 / 256 (auth), 128 (token provider/executor) / 128 bytes |
| Token counter | Each 0–1,000,000,000; invalid records dropped |
| Read window / response dimensions | 90 days / 2161 buckets, 1000 clients, 256 models |
| Storage operations | 250 ms ingest/maintenance, 2 s query, including storage-lock wait |
| Database | 262,144 pages (1 GiB with new default 4096-byte pages) |
| WAL | Autocheckpoint every 256 pages; 4 MiB journal-size target after reset |

Caps drop new events rather than silently coalescing identities/models or returning truncated totals. Lowering limits never truncates existing query results. These limits bound plugin memory/work; the official host may have already allocated an ABI input or queued a callback before delivery. The host's own manager has separate delivery/resource semantics.

Expired events are excluded from reads immediately. Ingest and minute maintenance delete at most 1000 expired rows per transaction; idle cleanup may lag expiration during large purges. Maintenance attempts a WAL truncate checkpoint; external readers can delay it. Freed main-database pages are reused, not automatically vacuumed. Retention is not a cryptographic erasure guarantee for WAL, filesystem snapshots or backups. Back up the database using SQLite's online backup tooling, or stop the host cleanly before copying; copying only the main file while WAL is live can omit committed data. [SQLite's WAL documentation](https://www.sqlite.org/wal.html) explains the sidecar and filesystem constraints.

Hot reload updates retention/caps in place, safely serializing with storage operations. `data_dir` is immutable for an open instance: restart to change it. A rejected reconfiguration keeps the current store working. Unload/shutdown drains queued work for about two seconds, counts remaining drops, checkpoints health best effort, closes SQLite, and releases the directory lock. Successful reads never delete usage.

## Delivery and health limitations

This is **best effort**, with no replay, exactly-once, crashproof, billing-grade, or historical-backfill guarantee. The async queue and SQLite WAL NORMAL mode can lose recent work on a process/machine failure. Host drops before callbacks are invisible to this plugin. Ingestion errors/queue overflow are acknowledged without surfacing an error to inference. The plugin does not intercept or modify inference traffic.

`collection.state` is `partial` when the requested window extends before collection started/retention, this instance has observed ingestion failures/drops, or the selected aggregate has unknown token events; otherwise it is `collecting`. This does not mean complete lifetime history. A disabled/unloaded plugin or unavailable host management is represented by HTTP unavailability, not a fabricated successful response with state `disabled`.

`duplicates`, `missing_execution_ids`, `dropped_events` and `ingestion_failures` are lifetime counters checkpointed during successful writes/maintenance/close. Drops include malformed/oversized/expired/future (>1 minute) records, missing IDs, capacity overflow, shutdown backlog, and failed ingestion. Maintenance faults increment failures without inventing dropped usage. Uncheckpointed counters can be lost on a crash or persistent storage failure. Counters do not measure omissions before installation or failures inside the host's delivery queue.

## Validation evidence and gaps

See [VALIDATION.md](VALIDATION.md) for the completed local checks and runtime assertions. Tests execute observable storage/plugin/HTTP behavior, including a real compiled shared library loaded by the official SDK builder and real backend management middleware. They do not substitute a fake loader or copy upstream code into a modified host.

Runtime tests use loopback-only inference with synthetic client/upstream/management keys. Model catalogs come from temporary local files, and the SDK's background npm request is sent to a local rejecting proxy. No external paid model APIs, push, deployment, global service configuration, or upstream edits are needed. Test temporary state is removed automatically.

Not verified: Windows/other architectures, real paid-provider traffic, Home connectivity, power-loss recovery, sustained production throughput, or unreadable failures in the upstream host. Home unavailability is established from the pinned backend source. Test/race coverage does not establish a crashproof durability claim.
