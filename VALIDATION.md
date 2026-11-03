# Local validation

Environment: Linux ARM64, Go 1.27.1, CGO enabled. Official read-only SDK checkout: `250d598175db61a4e99ac38b9164ed04745754e5`. All Go builds/tests use `GOMAXPROCS=2`, `-p 2`.

Completed checks on 2026-11-01:

- Actual C-shared build: `make build`; `build/client-usage-meter.so` is an ELF ARM aarch64 shared object, with the official ABI entry and buffer lifecycle exports.
- Behavioral Go tests: `make test`.
- Race detection of storage, lifecycle, queue and concurrent callbacks: `make race`.
- Static checks: `make vet` and `git diff --check`.
- Coverage is measured with `GOMAXPROCS=2 go test -p 2 -cover ./internal/...`; no coverage claim for C allocation glue. The final pricing implementation measured 85.3% in `internal/meter` and 83.1% in `internal/plugin`.
- Official-host runtime: `make runtime`.
- Official-host runtime with host race instrumentation: `make runtime-race` (ordinary `.so`; plugin logic separately instrumented by `make race`).

## Behavior exercised

Storage tests cover persisted HMAC identity and execution dedupe across restart; two-key global/filter reconciliation; native/plugin/provider namespaces; unattributed usage; retained deleted-key history; failed/successful retry executions; missing IDs; eight provider/accounting cases; cache-rate denominator and nullability; zero-only/unknown/inconsistent counters; UTC interval boundaries and empty buckets; retention and capacity recycling; lowered caps preserving existing reads; concurrent callbacks/queries; real SQLite write-lock faults; health checkpoint/restart; live database/WAL/SHM file permissions; secret canaries absent from response and stored bytes; symlink/broad-permission rejection; schema 1→3 and 2→3 migration and future-schema rejection; raw-key filter rejection; and pre-installation/retention coverage marked partial.

Plugin tests cover actual serialized ABI envelopes, registration and empty-Menu/no-Resources route declarations, nonconsuming reads, reconfiguration, failed configuration preserving the active store, malformed/oversized usage, invalid management methods/paths/query/body, bounded queue overflow, concurrent reload/callbacks, shutdown/reopen, and management errors not counting as dropped usage.

The integration harness imports the public SDK and starts its real proxy server. It loads the compiled library through the official native plugin loader, then sends synthetic client requests to the proxy and its loopback OpenAI-compatible upstream stub. The seed phase checks two successes, one genuine upstream failure, two distinct native clients, 240 total tokens, and three execution attempts. An independent named observer receives the same three callbacks. Re-publishing a captured execution increments duplicate count without changing totals.

Repeated authenticated reads preserve totals; per-client attempts sum to the global total. No-key/wrong-key/inference-key management reads return 401/403 through the backend's actual middleware. Public resource and data paths return 404. Invalid management queries return 400; writes are absent. A separate OS process reopens the same database, after removing one client key from the host configuration, and verifies stable IDs, retained history and duplicate suppression. Additional processes exercise priced usage and priced restart before a final process verifies management-disabled 404. The core usage-statistics aggregator is disabled throughout, demonstrating independent collection.

## Models.dev pricing verification

The pricing follow-up was implemented with failing tests first at its parser/arithmetic, persistence/API, configuration and conservative-tier seams. Initial tests failed for missing interfaces, missing tier selection, negative/duplicate rates, missing mappings, unsupported cache writes, and absent SDK configuration metadata. The old compiled library failed the new runtime pricing assertions before rebuilding. Final review added a failing test for missing rates in a selected context tier; the fix leaves those attempts unpriced instead of inheriting lower-tier rates.

Final verification uses the ordinary native `.so` through the actual official loader and management middleware. No modified host, fake plugin loader, production configuration, paid API, or user traffic is involved. Default unit/race tests have pricing disabled or use a private in-package fetcher seam and local HTTP fixtures; there is no production alternate-URL option.

Completed pricing checks on 2026-11-01:

```text
make verify
  Go tests: PASS
  in-process -race tests: PASS
  go vet: PASS
  C-shared build: PASS
  official runtime: PASS (seed, restart, priced, priced-restart, disabled)

make runtime-race
  five phases under the race-instrumented official host: PASS
  native library remains the ordinary build

MODELS_DEV_LIVE=1 GOMAXPROCS=2 go test -p 2 ./internal/meter \
  -run TestLiveModelsDevCatalog -v -count=1
  PASS: anonymous fixed public HTTPS endpoint, real parser and normalization
  response bytes: 5963821
  normalized bytes: 752466
  providers: 235
  normalized models: 8712
  SHA256: f2f089da079dbd94d318ccd2e4c6dd1941f9820342f2060397b91106a514c663
```

The normalized model count is smaller than the source's 8,940 because bounded identifier validation excludes unsupported identifiers. Unknown pricing dimensions stay marked unsupported. Rates in unit fixtures are synthetic arithmetic inputs only, never production defaults; the actual source catalog is neither bundled nor committed.

The public source and units were checked against [models.dev API documentation](https://github.com/anomalyco/models.dev/blob/dev/README.md), [official cost rules](https://github.com/anomalyco/models.dev/blob/dev/AGENTS.md#cost-always-usd), and the live [catalog](https://models.dev/api.json). The current API has flat `cost` rates and `tiers`, including duplicate legacy `context_over_200k` output for some non-200k thresholds. Tests assert explicit tier precedence and exact threshold behavior rather than interpreting that legacy name as a second band.

Pricing tests cover exact provider collision isolation; explicit provider and model alias maps in YAML/JSON-string configuration; missing versus explicit zero rates; USD-per-million conversion; OpenAI cache subsets, Claude independent buckets, Gemini separate reasoning, inclusive reasoning and separate reasoning rates; threshold equality/above-threshold behavior; legacy tier precedence; unsupported tier descriptors and modality dimensions; missing tier component rates; unknown cache-write TTL; zero-only, inconsistent and unclassified counters; resolved service-tier precedence; current versus historical versions; immutable estimates across refresh and restart; old unpriced metric JSON after v2 migration; two-client cost reconciliation; duplicate suppression; nullable/partial savings; disabled pricing preserving old estimates; age-based staleness; failed-refresh fallback; storage-gate failure status; catalog whitelist canaries; catalog/version retention; and concurrent callbacks, successful refreshes, reload, cancellation and close.

Network fixtures cover HTTP 200 enforcement, body limits, cancellation, rejected foreign/non-HTTPS/credential-bearing redirects, generic errors and a transport without environment-proxy forwarding. Parser checks include malformed/trailing JSON, duplicate object keys, negative/overflowing rates, empty catalogs, provider/model dimension bounds, and unknown tier properties. The production client enforces a 10-second total timeout and fixed public source.

The actual host emits the custom executor provider `openai-compatible-meter-stub`. The runtime test explicitly maps that ID to `openai`, asks the local inference stub for exact model `gpt-4o-mini` at `service_tier: default`, and waits for the public catalog to be ready. The resulting response has:

```text
priced_attempts=2
unpriced_attempts=4
estimated_cost_usd=0.000048000
estimated_cache_savings_usd=0.000006000
snapshot_versions=[f2f089da079dbd94d318ccd2e4c6dd1941f9820342f2060397b91106a514c663]
```

The four unpriced attempts are three original seed attempts and one synthetic premium-tier replay with a new execution ID. Re-delivery of a priced execution adds a duplicate, not cost. Per-client subtotals reconcile. A new OS process reopens SQLite and returns identical pricing metadata and event subtotals without repricing old events. Both original seed and priced wire responses were exported under this repository's ignored `build/` directory; no frontend fixture was edited.

The SDK fills omitted requested service tier with `auto`. An unresolved auto tier stays unpriced. A resolved standard response tier can establish standard pricing; the runtime fixture explicitly requests default service. This limitation is documented rather than guessed away.

Security/spec self-review confirmed: catalog URL fixed; no inference metadata or credentials on downloads; no environment proxy; bounded JSON/network/storage work; provider isolation; private management routes unchanged; catalog whitelist persisted in the existing owner-only WAL database; immutable deduped event costs; no retrospective history; current-cache status separated from selected-event versions; no unsupported cache-write/service/modality price claim. Full historical catalogs are not retained: one normalized current catalog plus referenced version metadata and immutable event subtotals bounds storage.

## Findings fixed during verification

- Dedicated storage permissions are enforced; tests create a private child directory instead of relying on platform-specific temporary-directory modes.
- As of 1.2.1, retention governs attempt history; event-count and fixed query-row caps are removed.
- Raw client keys cannot be echoed as selected-client values; filters accept only opaque IDs.
- Frontend integration review's coverage concern is addressed: requested windows before installation/retention are partial; zero buckets mean no observed records.
- The official host requires nonempty repository metadata. This local unpublished plugin identifies that status explicitly.
- A local management password alone is insufficient at this backend revision; the runtime fixture uses a synthetic configured secret hash.

## Frontend wire snapshot

The runtime harness can optionally export its sanitized seed-phase HTTP response for the sibling frontend's normalization and browser smoke tests:

```bash
METER_RUNTIME_SNAPSHOT=/absolute/path/to/usage-meter-official-runtime.json make runtime
```

For the additional priced phase:

```bash
METER_RUNTIME_PRICED_SNAPSHOT=/absolute/path/to/priced-runtime.json make runtime
```

Both variables can be set together. The output is real official-host/plugin wire JSON produced from isolated synthetic upstream traffic, not production data. The snapshot is written owner-only and contains only the public meter response, never the test keys, prompts or replay records. Omit the variable for ordinary verification.

## Limits of evidence

Trying a race-instrumented C-shared library in a race-instrumented host failed before assertions with ThreadSanitizer shadow-memory allocation errors (`errno: 12`, allocation of 103088128 bytes). This is **not a passing native-library race run**. The ordinary compiled library passes the real runtime under a race-instrumented host, and the same Go storage/plugin code passes in-process race tests. The production artifact is restored to the ordinary non-race build.

Home-mode unavailability is source-confirmed (`internal/api/server_management.go: managementAvailable`), not a live Home integration. Paid-provider traffic, other architectures, power failure and sustained production load are untested. The plugin makes no crashproof/exactly-once claim.

Verification stayed local. No publishing setup, push, PR, CI or deployment was added.

## Retention-only history (1.2.1)

`make verify` passed: unit tests, race tests, vet, native shared-library build, and all five official-host runtime phases. Regression coverage seeds the former 100,000-attempt capacity, ingests beyond it, restarts, deduplicates a repeated callback, ingests again, and verifies all 100,002 rows remain. It also checks default 90-day boundaries before and after maintenance, removal of the plugin's fixed page ceiling, and compatibility with old `max_events` YAML during registration and reload. Large-window query throughput is not benchmarked; the existing query deadline remains in effect.
