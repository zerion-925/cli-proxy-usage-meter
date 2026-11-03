# Additive models.dev pricing contract

This implements the agreed backend task without changing required fields, route, authentication, or `schema_version: 1`. No frontend files or sibling fixtures are modified.

`pricing` always includes:

| Field | Meaning |
| --- | --- |
| `available: boolean` | Selected window has at least one priced attempt, even if the price is explicitly zero or pricing is now disabled. |
| `source: "models.dev"`, `currency: "USD"` | Estimate source and units. |
| `status` | `loading`, `ready`, `stale`, `error`, or `disabled`; current catalog/worker state, independent of selected-event coverage. |
| `snapshot_version: string \| null` | Current cached response SHA256; not necessarily the selected events' version. |
| `fetched_at: string \| null` | Current snapshot's last successful retrieval timestamp, UTC RFC3339. |
| `snapshot_versions: string[]` | Sorted distinct versions used by selected priced attempts; empty array when none. |
| `priced_attempts`, `unpriced_attempts` | Coverage counts; their sum equals selected `totals.attempts`. |
| `savings_priced_attempts` | Optional agreed extension, always emitted by this implementation; may be smaller than cost coverage. |
| `estimated_cost_usd: number \| null` | Sum for priced attempts, null when `priced_attempts=0`. Partial coverage is a subtotal. |
| `estimated_cache_savings_usd: number \| null` | Sum for attempts with comparable read/input rates, null when `savings_priced_attempts=0`. |
| `reason: string` | Human-readable coverage, assumptions and current refresh limitations. |

Every Metrics object (`totals`, each timeline bucket, each model) additionally includes `priced_attempts`, `savings_priced_attempts`, `estimated_cost_usd`, and `estimated_cache_savings_usd`, with the same null/subtotal semantics. Model and timeline totals reconcile with the selected totals (subject to normal floating-point rounding). No extra `unpriced_attempts` field is needed in Metrics: subtract `priced_attempts` from `attempts`.

Old rows decode with zero coverage counts and null prices. Loading, failed or disabled pricing never turns old usage into a fake zero-dollar bill. A selected window can have estimates from multiple versions, including versions older than today's cached snapshot. Disabling downloads does not erase prior estimates.

Price assumptions: reconstructed token counters, standard text API list prices, exact provider/model mappings, output pricing for reasoning when no separate reasoning rate is published, and API-equivalent costs for OAuth/subscriptions. Cache writes use the published standard rate without a TTL requirement; missing rates, unsupported or unresolved service tiers, and unsupported pricing dimensions remain unpriced. Native Claude thinking is a subset of output. Metrics and pricing add `unpriced_reasons`, with pre-upgrade unpriced rows identified as `legacy_unpriced`. See README for configuration and bounds.

The existing `METER_RUNTIME_SNAPSHOT` export remains the original unpriced seed-phase response. `METER_RUNTIME_PRICED_SNAPSHOT` is an additional opt-in export of the actual priced official-host phase; use a path inside this repo unless separately authorized to update a consumer fixture. These exports contain only sanitized public response JSON.
