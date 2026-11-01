# Local validation

Environment: Linux ARM64, Go 1.27.1, CGO enabled. Official read-only SDK checkout: `250d598175db61a4e99ac38b9164ed04745754e5`. All Go builds/tests use `GOMAXPROCS=2`, `-p 2`.

Completed checks on 2026-11-01:

- Actual C-shared build: `make build`; `build/client-usage-meter.so` is an ELF ARM aarch64 shared object, with the official ABI entry and buffer lifecycle exports.
- Behavioral Go tests: `make test`.
- Race detection of storage, lifecycle, queue and concurrent callbacks: `make race`.
- Static checks: `make vet` and `git diff --check`.
- Coverage: approximately 82% of statements in both `internal/meter` and `internal/plugin`; no coverage claim for C allocation glue.
- Official-host runtime: `make runtime`.
- Official-host runtime with host race instrumentation: `make runtime-race` (ordinary `.so`; plugin logic separately instrumented by `make race`).

## Behavior exercised

Storage tests cover persisted HMAC identity and execution dedupe across restart; two-key global/filter reconciliation; native/plugin/provider namespaces; unattributed usage; retained deleted-key history; failed/successful retry executions; missing IDs; eight provider/accounting cases; cache-rate denominator and nullability; zero-only/unknown/inconsistent counters; UTC interval boundaries and empty buckets; retention and capacity recycling; lowered caps preserving existing reads; concurrent callbacks/queries; real SQLite write-lock faults; health checkpoint/restart; live database/WAL/SHM file permissions; secret canaries absent from response and stored bytes; symlink/broad-permission rejection; schema 1→2 migration and future-schema rejection; raw-key filter rejection; and pre-installation/retention coverage marked partial.

Plugin tests cover actual serialized ABI envelopes, registration and empty-Menu/no-Resources route declarations, nonconsuming reads, reconfiguration, failed configuration preserving the active store, malformed/oversized usage, invalid management methods/paths/query/body, bounded queue overflow, concurrent reload/callbacks, shutdown/reopen, and management errors not counting as dropped usage.

The integration harness imports the public SDK and starts its real proxy server. It loads the compiled library through the official native plugin loader, then sends synthetic client requests to the proxy and its loopback OpenAI-compatible upstream stub. The seed phase checks two successes, one genuine upstream failure, two distinct native clients, 240 total tokens, and three execution attempts. An independent named observer receives the same three callbacks. Re-publishing a captured execution increments duplicate count without changing totals.

Repeated authenticated reads preserve totals; per-client attempts sum to the global total. No-key/wrong-key/inference-key management reads return 401/403 through the backend's actual middleware. Public resource and data paths return 404. Invalid management queries return 400; writes are absent. A separate OS process reopens the same database, after removing one client key from the host configuration, and verifies stable IDs, retained history and duplicate suppression. A third process verifies management-disabled 404. The core usage-statistics aggregator is disabled throughout, demonstrating independent collection.

## Findings fixed during verification

- Dedicated storage permissions are enforced; tests create a private child directory instead of relying on platform-specific temporary-directory modes.
- Lowered event caps govern new ingestion and never make existing history unreadable.
- Raw client keys cannot be echoed as selected-client values; filters accept only opaque IDs.
- Frontend integration review's coverage concern is addressed: requested windows before installation/retention are partial; zero buckets mean no observed records.
- The official host requires nonempty repository metadata. This local unpublished plugin identifies that status explicitly.
- A local management password alone is insufficient at this backend revision; the runtime fixture uses a synthetic configured secret hash.

## Limits of evidence

Trying a race-instrumented C-shared library in a race-instrumented host failed before assertions with ThreadSanitizer shadow-memory allocation errors (`errno: 12`, allocation of 103088128 bytes). This is **not a passing native-library race run**. The ordinary compiled library passes the real runtime under a race-instrumented host, and the same Go storage/plugin code passes in-process race tests. The production artifact is restored to the ordinary non-race build.

Home-mode unavailability is source-confirmed (`internal/api/server_management.go: managementAvailable`), not a live Home integration. Paid-provider traffic, other architectures, power failure and sustained production load are untested. The plugin makes no crashproof/exactly-once claim.

The optional no-mistakes publishing gate could not be initialized: this new local repository has no origin, and `no-mistakes axi` reports it uninitialized; `no-mistakes init` requires an origin. No publishing setup, push, PR, CI or deployment was added. Local verification was run directly. The user explicitly required local-only work.
