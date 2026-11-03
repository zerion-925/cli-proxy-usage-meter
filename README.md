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
      max_clients: 1000
      max_models: 256
      pricing_enabled: true
```

Use the normal, CGO-enabled official proxy binary with `--config /absolute/path/to/config.yaml`. Keep existing inference credentials and other settings. `data_dir` can instead come from the **only plugin environment variable**, `CLI_PROXY_USAGE_METER_DIR`; explicit YAML overrides it. There is deliberately no temporary-directory or current-directory fallback. The host's `MANAGEMENT_PASSWORD` is also supported by its existing authentication middleware; it is not a plugin-owned variable. `pricing_enabled` defaults to true and enables anonymous public models.dev catalog downloads. Set it to false to stop downloads and pricing of new attempts; existing estimates remain readable.

At the pinned revision, management authentication requires a configured secret hash or `MANAGEMENT_PASSWORD`, **even when the SDK's local management password option is set**. A missing management configuration returns 404. Home mode blocks in-process management (404 through its availability gate; its heartbeat gate can return 503 earlier). Plugin collection and management availability are separate concerns. Core `usage-statistics-enabled: false` does not disable this independent sink, as verified at runtime.

The exact `/v0/management` path is the approved plugin-host exception, not a fallback for core management APIs. Registration contains GET usage and PUT client-label routes, empty `Menu` values, and **no `Resources`**. Do not add a Menu: the host turns it into an unauthenticated resource. The plugin metadata points to `https://github.com/zerion-925/cli-proxy-usage-meter`.

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
* **Price:** an immutable estimate using the last good models.dev snapshot available at ingestion. `pricing.available` is true only when the selected window has priced attempts. Unpriced history remains unpriced; a partial subtotal is never a full bill. See pricing details below.

The SDK treats reasoning as a subset of output for OpenAI-like providers, separate from output for Gemini-like providers, and independent for Claude-like providers. The plugin corrects the native Claude callback reconstruction: thinking is already included in output, so only input/cache buckets are independent. An explicit non-Claude executor retains SDK semantics. Actual original provider quality cannot be recovered from seven integers; malformed/incomplete source counters remain uncertain even if a plausible reconstruction is possible.

## Models.dev pricing

The fixed source is [`https://models.dev/api.json`](https://models.dev/api.json). The [official API documentation](https://github.com/anomalyco/models.dev/blob/dev/README.md) and [catalog authoring rules](https://github.com/anomalyco/models.dev/blob/dev/AGENTS.md#cost-always-usd) define prices in **USD per million tokens**. This community catalog is an estimate source, not an invoice or a historical price authority. No catalog or fallback prices are bundled in the plugin.

Startup starts a background refresh if the cached snapshot is missing, failed, or at least 24 hours old. Otherwise the next refresh is scheduled for its 24-hour age. Subsequent refreshes run every 24 hours, including after failures; there is no rapid retry loop. The client has a 10-second total timeout, a 16 MiB decoded-body cap, HTTP 200 requirement, strict JSON validation, and at most three redirects within the same HTTPS origin. A dedicated Go transport has no environment proxy, cookies, credentials, or host callbacks. Downloads contain no usage, model queries, or client identifiers. There is no configurable URL.

Failed validation/download/storage retains the last good catalog and marks pricing stale (or error if no catalog exists). `fetched_at` is the last successful retrieval timestamp in UTC, and `snapshot_version` is the SHA256 of that response body. A stale last-good snapshot can still price new attempts; it is explicitly reported as stale. Refresh errors are generic and never include network/configuration inputs. Disabling pricing keeps the cache and historical estimates.

Provider resolution is exact and deliberately small:

| Native usage provider | models.dev provider |
| --- | --- |
| `openai` | `openai` |
| `codex` | `openai` (API-equivalent estimate for subscription/OAuth usage) |
| `claude` | `anthropic` |
| `gemini` | `google` |
| Other/custom/OpenAI-compatible | Unmapped unless explicitly configured |

An exact model ID is required within that provider. No global lookup, reseller selection, fuzzy match, date stripping, or automatic alias guessing occurs. Mappings can override native mappings. Use the provider ID emitted by the pinned host: for a compatibility entry named `my-gateway`, it emits `openai-compatible-my-gateway`, not just the display name. The SDK executor type determines token accounting, never a pricing provider.

```yaml
# Within plugins.configs.client-usage-meter:
pricing_enabled: true
pricing_provider_map:
  openai-compatible-my-gateway: openai
pricing_model_map:
  openai-compatible-my-gateway/my-alias: gpt-4o-mini
```

This example is an explicit operator assertion that the gateway/alias should be estimated at that provider's price. It does not discover reseller billing. The model-map key is **native-provider/event-model** (split on the first slash); its value is the exact model ID in the mapped catalog provider. Both map fields also accept JSON object strings for the SDK's string configuration UI, for example `pricing_provider_map: '{"openai-compatible-my-gateway":"openai"}'`. Maps accept at most 128 provider entries and 256 model entries, validated bounded identifiers only; never put credentials or URLs in them. Updates affect new attempts only.

Cost uses the exported SDK's reconstructed, mutually exclusive uncached input, cache-read, cache-write, non-reasoning output and reasoning buckets, each divided by one million and multiplied by its applicable rate. Reasoning uses a separately published reasoning rate when present, otherwise the output rate; it is never added twice. A positive component needs a known rate; explicit zero rates are valid. Unknown, inconsistent, zero-only, or aggregate-only cache counters remain unpriced.

Context tiers use reconstructed **input total**, including classified cache reads and writes, with a strict `input_total > tier.size` threshold. The highest applicable tier supplies the rates. Missing rates for positive components in that tier remain unknown; lower-tier rates are not inherited. Explicit `tiers` always take precedence over the legacy `context_over_200k` representation; legacy-only entries use 200,000. Unsupported tier descriptors, unknown cost dimensions (including separate audio prices), and non-text output pricing are unpriced. Multimodal input without separate published prices is estimated under the standard text-token list-price assumption; the ABI cannot prove media composition.

The resolved response service tier takes precedence over the final outbound tier, then the requested tier. Only `default`, `standard`, or absent tier metadata can be estimated. An unresolved `auto`, `priority`, `flex`, or other tier remains unpriced. The host can label an omitted request tier `auto`; explicitly request standard service or rely on an authoritative standard response tier for coverage. Cache-write events use the published standard `cache_write` rate, following T3 Code’s estimator. The ABI does not identify TTL, so this is a standard list-price estimate, not a TTL-specific invoice. A missing rate still leaves positive cache-write usage unpriced.

Cache savings compare classified cache-read tokens at the actual uncached-input and cache-read rates from the same applicable tier. Both rates must be present; the signed difference is retained if reads cost more. Savings coverage can therefore be smaller than cost coverage. Attempts with known comparable rates and no cache reads contribute a known zero savings amount.

The schema remains version 1. [PRICING_CONTRACT.md](PRICING_CONTRACT.md) records the additive fields and frontend semantics. `pricing.available` describes the **selected events**, while `status`, `snapshot_version`, and `fetched_at` describe the **current cache**. `snapshot_versions` contains sorted distinct versions actually used by selected priced events. Costs and savings are null when their respective priced-attempt count is zero; otherwise they are subtotals for that coverage. `unpriced_attempts` includes missing mappings/rates, unsupported usage, disabled/loading periods and pre-pricing history. A ready current cache does not imply full coverage.

SQLite stores one current normalized catalog (maximum 8 MiB, 512 providers, 20,000 models, 16 tiers/model; rates 0–1 billion USD/million and thresholds 1–1 billion tokens). Original provider URLs, environment metadata, descriptions and other source fields are discarded. Version metadata (hash, first successful fetch, source and USD currency) survives while referenced by retained events or the current catalog; refresh and maintenance prune unused versions. Historical full catalogs are not accumulated: immutable event subtotals remain the historical evidence. Version metadata is bounded by retained events plus the current version. No historical effective-date or automatic repricing claim is made. All catalog tables use the same owner-only SQLite database and WAL rules as usage.

## Persistence, limits and security

SQLite uses the maintained CGO driver [`mattn/go-sqlite3` v1.14.52](https://github.com/mattn/go-sqlite3/releases/tag/v1.14.52), bundled SQLite, WAL, `synchronous=NORMAL`, a 100 ms busy timeout, one connection, immediate write transactions, secure delete, indexed time/client lookups, and versioned transactional migrations. Version 1 introduced events/metadata; version 2 adds trigger-maintained row/dimension counts so ingestion never recounts the event table. Version 3 adds immutable event price versions, a normalized current catalog, and referenced-version metadata. Migration keeps identity, dedupe and old metric JSON intact; old rows have null estimates. Newer unknown schema versions fail closed.

The directory must be absolute, owned by the running UID, a real directory, and mode 0700. Database, lock, WAL and SHM files must be regular, owned by the UID, and mode 0600; existing symlinks/hardlinks or broad permissions are rejected. New files use owner-only modes without changing the proxy process's umask. SQLite inherits the database permissions for its WAL/SHM files, verified with live storage tests. Use a dedicated trusted local directory and persistent volume, never a shared/untrusted or ephemeral mount. A nonblocking `flock` allows one plugin writer per directory; multiple processes/instances require separate directories.

A random 32-byte HMAC secret and random persisted instance identity live in the database. Client IDs use domain-separated, length-framed HMAC-SHA256 truncated to 128 bits over namespace + AccessProvider + APIKey. Labels contain only namespace and a short opaque suffix. Native/plugin providers and different plugin auth providers stay separate even if their key string is identical. Missing key or missing nonnative attribution is grouped as `unattributed`. Deleting/resetting the database changes identity; copying it to another independent deployment also copies its namespace, so provision separate databases for separate hosts.

Persisted event fields are only hashed execution ID, timestamp, opaque client ID, namespace, bounded model identifier, and whitelisted aggregate metrics. Keys/provider identifiers exist briefly in callback memory for hashing. The client-label table additionally stores a masked key prefix (at most eight characters, no more than one third of a key, with keys shorter than 12 characters fully masked) and an optional administrator-assigned name. Names are display metadata, not credentials. No raw keys, request IDs, headers, prompts, completions, bodies, failure text, credential/session/trace IDs, upstream URLs are persisted or logged by this plugin. Public catalog provider/model IDs, normalized rates, SHA256 versions, fetch timestamps, source/currency and event estimates are persisted. Mapping configuration stays in memory; it must contain identifiers only. Service-tier fields are bounded to 32 bytes, evaluated for pricing, and discarded. The model label accepts only a bounded identifier alphabet; invalid/secret-looking names or names containing the current key become `unknown`. Arbitrary model identifiers remain operational metadata, not a content-scrubbing guarantee. Raw keys are rejected as client filters. Host logging and other plugins remain the host operator's responsibility.

| Bound | Default / hard maximum |
| --- | --- |
| Retention | 90 days / configurable 1–365 |
| Retained events | No attempt-count cap; bounded by the retention window |
| Retained clients | 1,000 / configurable 1–1,000 |
| Retained models | 256 / configurable 1–256 |
| Callback queue | 256 records, nonblocking enqueue |
| ABI input | 1 MiB before copying into Go |
| Plugin YAML | 16 KiB decoded |
| Key / execution ID / provider / model | 4096 / 256 / 256 (auth), 128 (token provider/executor) / 128 bytes |
| Token counter | Each 0–1,000,000,000; invalid records dropped |
| Read window / response dimensions | 90 days / 2161 buckets, 1000 clients, 256 models |
| Storage operations | 250 ms ingest/maintenance, 2 s query, including storage-lock wait |
| Database | No plugin-imposed size ceiling; subject to available disk and SQLite limits |
| WAL | Autocheckpoint every 256 pages; 4 MiB journal-size target after reset |

Client/model cardinality limits still reject new identities/models beyond their configured bounds. Attempt count does not stop ingestion, and queries have no fixed row-count cutoff. Operation deadlines and queue limits still apply; large windows can exceed the query deadline rather than return partial totals. These limits constrain plugin work; the official host may have already allocated an ABI input or queued a callback before delivery. The host's own manager has separate delivery/resource semantics.

Expired events are excluded from reads immediately. Ingest and minute maintenance delete at most 1000 expired rows per transaction; idle cleanup may lag expiration during large purges. Maintenance attempts a WAL truncate checkpoint; external readers can delay it. Freed main-database pages are reused, not automatically vacuumed. Retention is not a cryptographic erasure guarantee for WAL, filesystem snapshots or backups. Back up the database using SQLite's online backup tooling, or stop the host cleanly before copying; copying only the main file while WAL is live can omit committed data. [SQLite's WAL documentation](https://www.sqlite.org/wal.html) explains the sidecar and filesystem constraints.

Hot reload updates retention/caps and pricing mappings in place, safely serializing with storage operations. Toggling pricing cancels and joins its download worker; close also cancels and joins it. Catalog network work and parsing run outside the storage gate. GET and inference callbacks never wait for a network refresh. `data_dir` is immutable for an open instance: restart to change it. A rejected reconfiguration keeps the current store working. Unload/shutdown drains queued work for about two seconds, counts remaining drops, checkpoints health best effort, closes SQLite, and releases the directory lock. Successful reads never delete usage.

## Delivery and health limitations

This is **best effort**, with no replay, exactly-once, crashproof, billing-grade, or historical-backfill guarantee. The async queue and SQLite WAL NORMAL mode can lose recent work on a process/machine failure. Host drops before callbacks are invisible to this plugin. Ingestion errors/queue overflow are acknowledged without surfacing an error to inference. The plugin does not intercept or modify inference traffic.

`collection.state` is `partial` when the requested window extends before collection started/retention, this instance has observed ingestion failures/drops, or the selected aggregate has unknown token events; otherwise it is `collecting`. This does not mean complete lifetime history. A disabled/unloaded plugin or unavailable host management is represented by HTTP unavailability, not a fabricated successful response with state `disabled`.

`duplicates`, `missing_execution_ids`, `dropped_events` and `ingestion_failures` are lifetime counters checkpointed during successful writes/maintenance/close. Drops include malformed/oversized/expired/future (>1 minute) records, missing IDs, capacity overflow, shutdown backlog, and failed ingestion. Maintenance faults increment failures without inventing dropped usage. Uncheckpointed counters can be lost on a crash or persistent storage failure. Counters do not measure omissions before installation or failures inside the host's delivery queue.

## Validation evidence and gaps

See [VALIDATION.md](VALIDATION.md) for the completed local checks and runtime assertions. Tests execute observable storage/plugin/HTTP behavior, including a real compiled shared library loaded by the official SDK builder and real backend management middleware. They do not substitute a fake loader or copy upstream code into a modified host.

Runtime tests use loopback-only inference with synthetic client/upstream/management keys. Host model catalogs come from temporary local files, and the SDK's background npm request is sent to a local rejecting proxy. The priced phase downloads only the public models.dev catalog over HTTPS. No external paid model APIs, push, deployment, global service configuration, or upstream edits are needed. Test temporary state is removed automatically.

Not verified: Windows/other architectures, real paid-provider traffic, Home connectivity, power-loss recovery, sustained production throughput, or unreadable failures in the upstream host. Home unavailability is established from the pinned backend source. Test/race coverage does not establish a crashproof durability claim.


## API-key tracking (1.2.0)

`clients` adds `name` and `key_hint`; `label` prefers a saved name, then a masked prefix.
HMAC identities remain unchanged. Equal prefixes never combine different keys.
`client_usage` contains each key's metrics and model names for the selected window;
its totals reconcile with the aggregate. Selecting a key filters all totals, model
breakdowns and timeline buckets. Keys are listed after their first observed attempt.
Historical keys gain a prefix on their next attempt; assigning a name works immediately.

Authenticated `PUT /v0/management/client-usage-meter/client-label` accepts
`{"client":"native_<32 lowercase hex characters>","name":"Team A"}`.
Names are at most 80 characters with no control or invisible formatting characters.
An empty name restores the masked-prefix display. Unknown clients return 404;
invalid input returns 400. No public resource route is registered.

Metrics and pricing summaries expose `unpriced_reasons` counts. Pre-upgrade rows
are reported as `legacy_unpriced`; stored history is never silently repriced.
The additive database migration creates `client_labels` and advances user_version
from 3 to 4. Back up the complete database before upgrading; restoring the old binary
requires restoring its matching pre-upgrade database as well.

Reference: [T3 Code pricing at 61c3794](https://github.com/pingdotgg/t3code/blob/61c3794c32b0eeebfac09add43e403618c65d2d7/apps/server/src/usage/usagePricing.ts).

Token summaries also expose `accounted_attempts`, `uncached_input_tokens`,
`accounted_cache_read_tokens`, and `accounted_cache_rate`. The latter rate uses only
fully classified attempts, and its coverage must be displayed with the value.
The original `cache_rate` keeps its all-or-nothing semantics for older consumers.
Already-classified historical rows can recover uncached input from their saved
input denominator minus cache reads and writes. Unknown rows remain excluded.

## Retention-only attempt history (1.2.1)

Attempt history expires by age (90 days by default). The former `max_events` setting is removed from plugin metadata and is ignored in existing YAML for compatibility. The one-million-row query cutoff and plugin-imposed 1 GiB database ceiling are also removed. Valid attempts are no longer dropped merely because the retained event count reached a threshold.

This change preserves the existing per-attempt storage and saved estimates. It does not reset history, change aggregation, or change the retention setting. Expired rows are still removed in bounded batches and excluded from queries immediately. Disk capacity, SQLite's own limits, client/model cardinality limits, operation deadlines, and best-effort callback delivery still apply.
