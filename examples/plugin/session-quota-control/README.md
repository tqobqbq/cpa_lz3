# Session and quota control

A Go C ABI plugin for local CLIProxyAPI routing, with a browser dashboard for
manual session assignment, a fixed delay per request for Codex/Claude five-hour quotas,
and a persistent usage report with request and wait history.

## Credential and API key access lists (0.7.0)

The **访问控制** tab (`#access`) edits permissions from either a specific
credential/account or a downstream API key. Each side chooses exactly one mode:
`blacklist` or `whitelist`. The default empty blacklist allows everyone; an empty
whitelist denies everyone. Both directions must allow a pair. Keys are displayed
and saved in full, and credentials are identified by their stable `auth_index`.

Rules live under `plugins.configs.session-quota-control.access_control` in
`credential_rules` and `api_key_rules`; each rule has `mode` and `entries`.
See [config.example.yaml](config.example.yaml). Search, multiple selection, draft
retention and a combined permission preview are available in either view. The
Credential table also provides an access-control shortcut. Removed identities
remain visible when referenced by rules; rotating a key creates a new identity.

Local candidates are filtered before priority, round robin, native affinity and
manual assignment. Manual queues are isolated by client key. Permission changes
are checked again after manual/quota waits and before credential refresh retries
in non-streaming, streaming, WebSocket and token-count execution. Requests already
sent upstream finish normally. If every candidate is denied, the response is
`403 credential_access_denied`. Filter failures return
`503 credential_access_filter_failed` without unfiltered fallback. The plugin's
own management-initiated checks without a client key retain administrative
access; nested requests with an authenticated client still enforce the rules.

Rebuild both the plugin and the matching server for RPC schema 8.

## Build and enable

Run from the repository root:

```bash
bash examples/plugin/session-quota-control/build.sh
```

The script builds the plugin and matching server in `bin/session-quota-control/`,
then publishes the same plugin to `plugins/<GOOS>/<GOARCH>/`. Both plugin copies
are updated on every successful build. CGO and a C compiler are required. Use the
newly built server: the plugin requires RPC schema 8, including the host extensions
implemented in this repository.

Merge [config.example.yaml](config.example.yaml) into the existing server
configuration, preserving its other fields. Run the server from the repository
root so the relative plugin directory resolves correctly:

```bash
./bin/session-quota-control/cli-proxy-api --config config.yaml
```

Alternatively, `bash a.sh` rebuilds both artifacts and starts the matching server
with `config.yaml`. It forwards additional command-line arguments to the server.

Enable native `routing.session-affinity`, global plugins, and this plugin. Local
routing is required; Home owns scheduling and does not use the local admission
gate. An independent scheduler plugin can also bypass the native affinity
selector and its admission gate.

Open the **Session & quota control** plugin menu, or navigate directly to:

```text
/v0/resource/plugins/session-quota-control/dashboard
```

The dashboard includes a **Hub** tab (`#hub`) alongside scheduling, credentials,
and usage. The HTML is a public resource and contains no session or credential data. Sign in
to the main management UI on the same origin (scheme, host, and port). The server
adds a restricted bridge when serving `/management.html` and binds the bundled
UI's current authentication store. The embedded dashboard asks its parent page
to make plugin API requests using the current management connection. It does not
need a separate cookie handshake or another login. The parent never returns the
key to the plugin iframe. Login and logout changes take effect through the same
store. The downloaded asset is left intact; no plugin local/session storage holds
the key. Other frontend builds retain request-observation fallback behavior.

Standalone pages can reuse an encrypted HttpOnly browser cookie. It lasts up to
12 hours, is encrypted with a process-local key, and can
only authenticate v8 plugin extension and per-plugin configuration endpoints.
Every request still validates the original management key and remote-access
policy. Server restarts, expiry, and key changes invalidate this fallback cookie;
the embedded page continues to use the main UI's current valid connection.
The main UI's logout/login-route and unauthorized events clear the cookie.
Use the same origin consistently; `localhost` and `127.0.0.1` do not share cookies.
An external management panel is not bridged. The dashboard has no login-status
card after connecting. Login recovery controls appear only when authentication
fails, including a collapsed Management Key form that keeps the supplied key in
page memory only.

Scheduling inputs show plugin defaults immediately and switch to the current
server configuration after connecting. Settings use the v8 configuration API
and the normal configuration watcher. There is one plugin menu and one page with
**会话调度**, **Credential 管理**, **访问控制**, **用量统计与请求记录**, and **Hub** tabs. They share a connection and keep
their filters, pagination, expanded diagnostics, and drafts when switching tabs.
Only the visible tab polls. Open the usage tab directly at:

```text
/v0/resource/plugins/session-quota-control/dashboard#usage
```

The old `/report` resource opens the same page with usage selected. Native login
sharing and the unified page require both the matching server and plugin (0.3.2
or newer). After updating, reload the main management page once to load its bridge.

### Native credential quota updates

While the main management page is visible and logged in, its bridge reads
`GET /v8/management/credentials/quota-observations` every two seconds and when the page regains focus.
It feeds Codex and Claude response-header observations into the native quota cache,
using the native credential identity helpers and window parsers. Older observations
cannot replace newer API-query results, and windows absent from the observation
keep their existing values. This reads server memory without querying providers.
Hidden pages and logged-out connections do not poll.

The Credential tab in plugin 0.4.0 or newer shows remaining allowance from
the latest native response observation. Its two-second state refresh reads only
host memory and includes all reported windows: five-hour, weekly, code review,
model-specific and other limits, plus credit balances when present. A reported
used percentage is converted to its remaining percentage; missing values stay
unknown. Each credential exposes its observation time, resets and all retained
quota fields. The original calculation snapshot remains in request details.
Display updates do not change an already scheduled request's wait.

### Credential evaluations (0.5.0)

Each Credential row has a **降智检测** button. Its dialog shows historical and
scheduled tests, with **新增检测** for selecting a model, reasoning effort, test
types and execution times. Defaults are `gpt-6-astra`, `low`, numeric fingerprint
and pelican, at submission and five minutes after submission: **four test jobs**.
The model name is editable and is sent exactly as entered. It must be available
on the selected credential; the plugin does not silently substitute another model.
Version 0.5.1 corrects the default model spelling. Existing records keep their
original model name; create new tests after updating.
Additional delays can be entered in minutes (1–1440), up to eight selected times.

The three tests are adapted from `cpa-plugin-codex-candy-eval` v0.4.2:

- Candy preserves its original prompt and standalone-`21` grading, including its
  limitations: it does not validate reasoning or the final answer.
- Numeric fingerprint copies the complete Go classifier, all 36 challenges,
  frozen reference bank and calibration. Each job samples three challenges and
  produces ranked candidates and probabilities when all three answers are valid.
  The supplied effort applies to every challenge. Unsupported names/aliases in
  the reference bank have no match verdict; their rankings remain visible.
- Pelican preserves both the original HTML/SVG animation and the all-in-one SVG
  prompt. The model replies with code. An isolated browser frame displays it;
  the plugin does not execute Codex tools or edit workspace files.

The default four jobs normally make eight model requests before retries.
Each request is pinned to the selected credential through the host model
callback, which retains responsibility for translation and final payload rules.
Invalid answers and transient failures can retry up to three attempts; wrong
candy answers, credential rejection and rate limiting do not trigger retries.
Streams have no elapsed-time deadline. Cancel a queued or active test from its
record. There are four active job slots and six active model-request slots.

Jobs and their evidence are persisted separately from usage reporting in
`evaluation_db_path` (default `~/.cli-proxy-api/session-quota-control/evaluations.sqlite`).
Closing the browser does not cancel work. Pending jobs survive plugin/server
restart and become eligible at their original timestamp; concurrency can delay
their actual start. Interrupted active jobs are recorded, not automatically
replayed. Credentials are rechecked at execution time. Each credential keeps its
100 most recent finished records, plus all pending and active records.

The authenticated extension exposes `GET/POST /evaluations`,
`GET /evaluations/catalog`, `GET /evaluations/record` and
`POST /evaluations/cancel`. Histories are scoped by `credential_id`; detail and
cancel requests also require the record `id`. Record summaries omit response
text, generated HTML and challenge details.

The algorithms, data provenance and MIT licenses are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Fingerprint parity fixtures
verify the copied rankings against the pinned upstream detector.

### Hub channels (0.6.0)

The Hub tab saves Authorization, KEY_ID, model, effort, protocol, endpoint URLs,
project, proxy, optional cookies, model filter and selected channel IDs on the
server. Saved secrets are never returned to the page; blank inputs retain them.

Discovery and detection are separate operations. **Find free channels** only
lists public, enabled channels with known zero prices. It does not require a key
ID or call a model. Detection accepts selected or manually entered IDs, or the
key's current explicit bindings, without requiring discovery first.

For each of up to 100 selected channels, detection temporarily pins the key to
that channel, verifies the binding, and waits 500ms for propagation. It reuses
this plugin's existing numeric fingerprint challenges, reference detector,
calibration, aliases and retry rules through a Hub HTTP transport. The transport
supports Responses and Messages; automatic protocol selection uses Messages for
model names containing `claude`. Messages maps `minimal` effort to `low` and
`xhigh` to `max`. Tools are not executed and model streams have no time deadline.

Completion, failure or manual cancellation restores the original profiles.
Closing the page does not cancel a job. **Apply passing channels** is an explicit
action, accepting only complete three-challenge results that match the requested
model. It updates the active profile's explicit binding and removes alternate
tag, dynamic and fallback routes, while retaining quota, model mappings, provider
restrictions and other profiles. Unknown models and partial results do not pass.

The settings, channel list, last ten batches and a write-ahead recovery snapshot
are stored in a `0600` file at `evaluation_db_path + ".hub.json"`, independent of
usage reporting. After interruption, use **Restore / check binding** before a
new operation. External profile changes are not overwritten. Authorization,
cookies and proxy settings can still be updated during recovery. Only one Hub
operation runs at a time. This feature uses the detector bundled in this plugin.

### Credential table and appearance

The Credential tab follows the native Credential Management table's identity,
plan, availability, recent requests, historical usage, quota, and action columns,
with an additional pacing column. Provider tabs, search, status/plan/quota filters,
sorting, and optional email masking apply to the complete credential list.
Credentials without traffic remain visible. Only observed plan and quota data
are shown; unavailable metadata is not guessed from file names.

The previous quota/pacing and per-credential wait tables are consolidated here.
Each row opens a detail dialog or a wait-settings dialog with independent maximum
and fixed delays. Unsaved drafts survive polling and tab changes. Waiting counts,
the last decided delay, model cost, historical average concurrency, and the next
resume time remain available. Changes apply to subsequent selections only.
The native credential enabled/disabled state is displayed; credential lifecycle
operations remain in the native management UI.

Historical counts, tokens, success rate and last request come from this plugin's
existing SQLite execution records, including retries, cached for five seconds.
They cover activity since plugin collection began, not the native manager's
possibly different retention range. If collection is unavailable, runtime counts
are shown when provided by the host and tokens remain unknown.

The shared dashboard uses the native frontend's typography, light/dark palette,
status badges, quota bars, rounded surfaces and credential row layout. Embedded
pages follow the parent manager's theme; standalone pages follow the system theme.

Native Credential Management synchronization is implemented in the server bridge
and works with an installed 0.3.2 plugin. The scheduling display change requires
replacing the plugin with 0.3.3; it adds no new host API requirement.

## Manual assignment

Native affinity checks explicit client identities, parent/fork bindings and
content matching first. A cold binding is exposed in the dashboard and waits up
to `assignment_wait_seconds` (60 by default). Select an eligible credential or
choose automatic routing immediately. Expiration resumes the original selector,
including priority and round-robin behavior.

When a manually selected credential fails and another credential is needed, the
request returns to manual assignment. This also applies to later requests that
reuse that manual binding and to inherited manual bindings. The dashboard shows
the previous credential. An assignment timeout or choosing automatic routing
delegates the new binding to native scheduling; subsequent automatic retries do
not reopen manual admission. Turning off manual assignment also releases retries.

The native selector stores manual choices in its existing cache/matcher. Manual
assignment can select an eligible lower-priority credential. The host rechecks
credential availability and model support after the wait. A manual choice that
became unavailable returns to assignment when other candidates remain. The list
comes from native provider, model, access and availability filtering, excluding
credentials already tried by this request. Automatic fallback still selects the
highest available priority tier. Existing reusable
bindings pass immediately. Concurrent requests for one identity reuse the first
binding. Requests without a usable identity can be assigned individually; stable
subsequent affinity requires an explicit or inferred identity.

Turning off `manual_assignment_enabled` releases pending decisions. Setting the
wait to zero immediately uses automatic routing. Downstream cancellation cleans
up waiting decisions and quota tickets. Manual decisions check cancellation every 250 ms. Quota delays use one timer
that is immediately canceled when the downstream disconnects. Neither path sets
network deadlines on upstream connections.

### Session diagnostics

Expand **粘性判断字段** on a pending row to inspect the native selector's
identity inputs and lookup values: session/thread and parent identifiers from
headers and payload fields, extraction results, execution and derived metadata,
provider, original and canonical model, exact cache keys and binding state, TTL,
subagent inheritance, rebind policy, and LCP namespace/fingerprints. Only supplied
identity fields appear in the input maps. Content matching is identified separately
from explicit client and WebSocket connection identities.

The dashboard retains the last 50 completed admission records in memory, including
their diagnostic fields and manual, automatic, timeout, or cancellation result.
These records cover requests that reached manual admission; ordinary native cache
hits do not create history entries. Neither polling nor expanding details changes
session bindings or quota snapshots. Request authorization headers and full message
bodies are excluded. Diagnostic data requires the existing management authentication.

## Five-hour quota pacing

After credential selection, automatic mode reads the native passive quota snapshot
**once**, calculates a duration **once**, and waits on **one timer** before upstream
execution. HTTP, streaming and WebSocket executions use the same interception
path. Running timers are not changed by later observations, configuration writes,
new requests, or usage records. Downstream cancellation and plugin unload can
interrupt a timer.

Timers run independently, including requests assigned to the same credential.
Three requests arriving together with a fixed 30-second delay can proceed together
after about 30 seconds; their delays do not become 30, 60 and 90 seconds. Historical
concurrency influences the automatic estimate, but does not create a serial queue
or stagger the timers.

The native observation formats are:

- Claude: `anthropic-ratelimit-unified-5h-utilization` and `...-5h-reset`.
- Codex: windows explicitly identified as 300 minutes, with used percentage and
  absolute or relative reset time, including native WebSocket quota observations.

Pacing never polls quota during a wait. The dashboard independently reads cached
response observations for display, without querying upstream or changing timers.
With no valid, current five-hour observation, automatic mode uses zero delay.
Weekly and other windows are displayed but do not enter the five-hour formula.

Let `C` be the model's average quota fraction per request, `N` the credential's
historical average concurrency, `R` the observed remaining fraction minus the
configured safety margin, and `T` the time until reset:

```text
sleep = T * min(1, C * N / R)
```

When `R <= 0`, the duration is `T`. For multiple applicable five-hour windows,
calculate the duration for each and take the largest. Automatic durations are
bounded by the reset horizon (and at most 24 hours). There is no FIFO scheduling,
quota reservation, or concurrency gate. Every request independently sleeps its
chosen duration.

### Model consumption

`C` is the arithmetic moving average of the last `model_average_samples`
attributable request costs (default 100), grouped by provider and upstream model
across **all credentials in this process**. Reasoning suffixes are removed from
the model identity. Body length, token counts, output token limits, reasoning
budget, effort and service tier are not inputs to the estimate.

Usage callbacks learn costs from changes in response quota percentages. For an
interval containing `k` completions of one model and a measured quota increase
`d`, each completion contributes `d/k` to that model's moving average. Repeated
rounded percentages accumulate completion counts until they change. Intervals
containing multiple models cannot be attributed reliably and are skipped. Resets,
duplicate records and out-of-order records do not create consumption samples.
Learning continues while automatic pacing is disabled or a fixed override is in
use. No extra quota lookup is made for learning. A model with no measured samples
uses `initial_request_percent` (default 1% per request).

### Historical concurrency

`N` is the time-weighted average number of selected, unfinished requests on that
credential over the last `concurrency_window_seconds` (default 300 seconds).
Sleeping requests are included. The calculation uses the recorded history,
not the instantaneous count at admission, with a minimum estimate of 1. Native
request completion and cancellation events update the history.

### Per-credential fixed waits and caps

Use `credential_waits` keyed by stable **auth index** (shown on the dashboard),
or by credential ID when configuring YAML manually:

```yaml
credential_waits:
  "0123456789abcdef":
    max_wait_seconds: 120
    fixed_wait_seconds: null
  "fedcba9876543210":
    max_wait_seconds: 60
    fixed_wait_seconds: 30
```

- `fixed_wait_seconds: null` or omitted: automatic calculation.
- `fixed_wait_seconds: 30`: directly sleep 30 seconds, without reading quota or
  evaluating model consumption or concurrency. Overrides can be used on any provider.
- `fixed_wait_seconds: 0`: immediately continue.
- `max_wait_seconds: null` or omitted: no additional cap.
- `max_wait_seconds: 10`: truncate both automatic and fixed waits to 10 seconds.
- `max_wait_seconds: 0`: immediately continue.

Settings accept whole seconds from 0 to 86400 and can be saved independently for
every credential in the dashboard. The cap is applied after choosing the automatic
or fixed duration. Changes apply to subsequent selections. Global
`quota_pacing_enabled: false` bypasses all quota and fixed waits.

A duration computed from historical averages is predictive, and fixed overrides
or caps can shorten it. No guarantee is made that a provider's quota will never
be reached. State, learned averages and concurrency histories are per process and
are cleared on restart; configuration and the SQLite usage report are persisted.

## Usage report

The implementation adapts CPA Usage Report v0.3.1's SQLite event storage,
date ranges, breakdowns, and token accounting. Attribution
and the MIT license are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
The page uses this plugin's lightweight HTML/JavaScript dashboard rather than
shipping a second React build. No separate reporting plugin or service is needed.

The report provides:

- Request count, completed success rate, token totals, and upstream execution count.
- Hour/day/month token and actual wait trends, with model/provider/outcome/search filters.
- Model, upstream provider/credential and downstream plaintext API-key breakdowns,
  with paginated request history.
- Manual-assignment waits, including reselection after failure and timeout fallback.
- Each credential selection's planned quota delay, actual timer duration, calculation
  reason, model cost, concurrency estimate and the snapshot used by the calculation.
- Per-execution token usage, latency, TTFT, recorded reasoning effort and the model
  reported by the upstream response, including failed and retried executions.
- Request rows and details show the source API key, client IP and User-Agent separately from
  the upstream provider and credential. Keys are stored and displayed in plaintext.
- Token totals, cache-read rates, API unit prices and reference costs, with TTFT
  and both TPS definitions (including/excluding TTFT).
- Cancellations before upstream execution, even when no token usage event exists.

Request history uses status badges, labeled token counts in one column, separate timing labels,
and 25/50/100-row pagination. The request inspector separates the overview,
unit prices and costs, execution timeline, and raw JSON.
The timeline orders admission, credential selection, and
upstream execution events by their start times; failed executions expand automatically.
Request, trace, session, and credential IDs can be copied, as can the raw JSON.
The layout supports light/dark themes, narrow screens, and keyboard navigation.
The information hierarchy draws on [cpa-usage-stats](https://github.com/tom2almighty/cpa-usage-stats),
[CPA Usage Statistics](https://github.com/Oniokey/CPAUsageStatistics), and
[CPA Usage Report](https://github.com/andyWang1688/cpa-usage), using this plugin's
existing request lifecycle data and accounting rules.

Upstream identifies the provider and credential used to call the model. Downstream
identifies the client calling this service with an API key. The two dimensions
aggregate the same recorded executions: provider/credential groups attribute each
attempt to its destination, while client-key groups count each request once and
sum all its attempts, including retries. The source key comes from authenticated
request context, independently of upstream Authorization headers. Client IP follows
the host's existing trusted-proxy resolution, falling back to the direct peer.
Pre-execution canceled requests retain source identity. Historical hashes cannot be
reversed and are explicitly labeled as legacy anonymous identifiers; missing IPs
are not inferred from headers or older records. The original downstream User-Agent
is captured from request context before upstream header rewrites, persisted even
for requests canceled before execution, and shown in the source column and detail
view with a copy action. Historical records without it remain uncollected.
Key, IP and User-Agent are searchable. The dedicated `user_agent` filter performs
a case-insensitive substring match and combines with other filters; summaries,
distributions, trends and pagination all use the same filtered request set.

Token totals use canonical, non-overlapping input/cache/output/reasoning buckets.
Cache-read rate is cache-read input / total input, including cache writes; aggregates
divide summed counters rather than averaging per-request percentages. TTFT averages
successful samples with positive timing no greater than execution duration. TPS excluding TTFT is output tokens / (execution duration minus TTFT). TPS including
TTFT is output tokens / execution duration. Summary cards, every breakdown, request
rows and execution details display both with explicit labels. Both
exclude scheduling before execution and divide summed eligible counters. Missing
usage or timing remains unknown; output includes reasoning tokens.

The bundled `prices.json` snapshot comes from [models.dev](https://models.dev/api.json)
and contains 235 official-provider text model entries. Prices are USD per million
tokens, with context-length tiers where supplied. The report shows input, output,
cache-read and cache-write rates, snapshot date and reference costs. Only exact
model IDs (including recognized provider prefixes and thinking suffixes) are matched;
unrecognized aliases, missing rates and incomplete usage remain unpriced. The
snapshot is not refreshed at runtime. Historical records are estimated using this
snapshot, not the rates in effect when the request ran. Matching OpenAI models use
the official service-tier snapshot described below. Estimates describe model- and
tier-specific text API usage, not subscription/OAuth charges, reseller bills, batch
discounts, or image/audio/tool fees. Different breakdowns describe the same usage and must not
be added together. Totals preserve an explicit pending-cost count and reason counts
for unmatched models, missing applicable rates, missing billable usage and incomplete
token accounting. Pending amounts are excluded from the displayed subtotal; they
are not zero-cost calls. Failed retries without usage can also remain pending.

The usage trend switches between tokens, downstream requests/upstream executions,
reference costs, and waiting time. It follows the current filters across all pages,
with hourly, daily or monthly intervals in the server timezone. Idle intervals are
filled with zero; future intervals are omitted and the current interval is partial.
Hourly intervals retain UTC offsets through DST changes. Hover, tap, or use the
arrow keys to inspect exact interval values, including cache counts and rates.
Cost lines show only priced subtotals; outlined markers identify intervals with
pending costs, and wholly unpriced intervals leave a gap instead of implying free
usage. Requests and all their retries are attributed to the request start time.
Selecting Today defaults to hourly intervals. Queries are limited to 5,000 trend
intervals; use a coarser interval or shorter range for longer periods.

Request rows, the overview and each execution show the final outbound service tier:
Standard (`default`), Fast (`fast` / `priority`), Ultra-fast (`ultrafast`), Flex,
Auto (omitted or `auto`), or the unrecognized raw value. The host captures
`outbound_service_tier` immediately before HTTP transport or after WebSocket payload
rules, so filtering `service_tier` records Auto and an override records its final value.
Client-requested and upstream-reported tiers are retained separately in details.
Historical records without an outbound tier are labeled as not collected.
For ordinary API requests, the reported tier takes precedence for
reference pricing, including a Fast request downgraded to Standard. Codex OAuth
responses are different: `default` / `auto` do not confirm Standard processing
and can appear when Fast is active. In that case pricing uses the final outbound
tier as an **unconfirmed estimate**, preserving the raw response value in details.
Historical records fall back to the client-requested tier with an explicit source
label; their previously unrecorded outbound payload cannot be reconstructed.
Known API-key credentials retain normal response precedence.
See the [OpenAI explanation](https://github.com/openai/codex/issues/14204#issuecomment-4033184620)
and [CPA verification guidance](https://github.com/router-for-me/CLIProxyAPI/issues/6460#issuecomment-6061141939).
Each retry uses its own tier and estimation basis, and its adjusted
cost flows into all summaries, distributions and trends.

`service_prices.json` contains the [official OpenAI text API price snapshot](https://developers.openai.com/api/docs/pricing) for
27 models, including 42 Fast, Ultrafast and Flex tables. These override matching
standard prices from `prices.json`; other models retain the models.dev snapshot.
Rates depend on the model and context threshold, not a universal multiplier.
Missing tier or context rates remain pending. When no response tier was recorded,
explicit outbound tiers are estimates; missing or Auto tiers use a labeled
Standard reference basis. Aggregate notes count priced calls whose tier is
unconfirmed. Old records cannot reveal a previously unrecorded tier.
OpenAI cache writes use the 30-minute reference rate because collected usage does
not distinguish 12-hour writes. These are API reference costs, not subscription
allowance or purchased-credit multipliers. Sources and snapshot dates are shown.

Reasoning effort and speed are independent. Effort does not directly multiply
the token price. Codex Ultra is a client orchestration mode that permits proactive
subagents; it is not the Ultrafast service tier. Client turn metadata may include
`reasoning_effort`, but it is not a guaranteed response header or proof of the
upstream's effective effort. The report keeps using host-reported execution effort.

Request rows and usage breakdowns show input, uncached input, cache reads, cache
writes, output and reasoning in one text column without composition bars. Input
includes cache reads/writes; output includes reasoning, so these subtotals must not
be added again. Request rows and the inspector show the recorded effort and upstream
response model from the latest execution for the final credential; the timeline
retains every attempt's values. These fields are copied from host usage events,
not inferred from requested model names or thinking suffixes. Missing historical
or upstream-reported values remain unavailable. Search includes both fields.

A lifecycle UUID groups admission, credential selections, usage events and completion.
Retries count once as a request; their actual upstream executions, token counters
and waits accumulate underneath it. Execution UUIDs deduplicate usage callbacks.
No session-ID or short HTTP trace-ID heuristic is used for this join, so separate
requests and WebSocket turns remain distinct. Executions outside the handler
lifecycle appear separately with an `execution:` ID; they have no measured manual
or quota waits.

Manual and quota waits are measured separately. A canceled 60-second timer may
have only 10 seconds of actual waiting. Manual wait includes time until a manual
selection, automatic release, timeout or cancellation, including reassignment.
The five-hour plan is the chosen delay after fixed overrides and caps; actual
quota wait is the measured timer duration. The report accumulates every relevant
attempt within the request. Five hours describes the quota window, not a constant
wait duration. Running requests include the elapsed time of active waits at the
report snapshot; missing timer results after request completion are shown as unknown. Quota plans are never recalculated by the
report. The report reads stored data only and does not fetch quotas or consume the
native usage queue. Live elapsed times are display values, not scheduling inputs.
Total time covers the model request lifecycle; executor latency excludes the waits
before execution. It is not a measurement of the client's network latency.

The request table groups quota actual/planned times and any manual-assignment waits
in one waiting-time cell; full admission records remain available in the inspector
and aggregate reports. Source API key, IP and User-Agent have a dedicated column, separate
from the upstream provider/credential column. Expanded details show full plaintext
keys with a copy action. Raw JSON also includes source identity.

SQLite defaults to `~/.cli-proxy-api/session-quota-control/usage.sqlite`, using WAL
and normal synchronous mode. New records survive ordinary restarts. A request
left running by a previous plugin instance is shown as interrupted; unfinished
timers are unknown rather than treated as fully slept. No historical wait data
from before this feature can be reconstructed. Records are retained until the
database is explicitly removed; changing its path switches to a separate database.

Storage settings can be changed on the report page. Turning collection off does
not delete the database. Write/read errors are shown on the page; storage failures
do not reject model requests. Request/response bodies and response headers are not
stored. The database stores plaintext client API keys, client IPs, User-Agents, session and
credential identifiers, and is separate from CPA Usage Report's database.

The host propagates lifecycle identity through metadata/context and affinity/usage
callbacks, forwards canonical token accounting, and includes authenticated source
API key, resolved client IP and original User-Agent in request/usage callbacks. Rebuild and run the
matching server and plugin together to collect source identity for new requests.
Earlier hashed keys, missing IPs and missing User-Agents cannot be backfilled.

## Configuration

| Field | Default | Meaning |
| --- | --- | --- |
| `manual_assignment_enabled` | `true` | Wait for cold session assignment. |
| `assignment_wait_seconds` | `60` | Manual decision deadline, 0–3600 seconds. |
| `quota_pacing_enabled` | `true` | Enable automatic/fixed quota waits. |
| `safety_margin_percent` | `5` | Deduct this percentage from remaining quota. |
| `initial_request_percent` | `1` | Per-model fallback request cost before measured samples. |
| `model_average_samples` | `100` | Recent attributable request costs per provider/model. |
| `concurrency_window_seconds` | `300` | Historical concurrency lookback, 1–3600 seconds. |
| `max_pending` | `1024` | Maximum pending manual decisions and sleeping requests. |
| `credential_waits` | `{}` | Per-credential fixed durations and caps described above. |
| `usage_report_enabled` | `true` | Persist and display usage and waiting records. |
| `usage_db_path` | `~/.cli-proxy-api/session-quota-control/usage.sqlite` | This plugin's SQLite database. |

The former `reservation_multiplier`, `reference_request_tokens`,
`default_output_tokens` and `max_in_flight` settings are no longer used.

## APIs and host changes

Authenticated routes:

```text
GET  /v8/management/plugins/session-quota-control/extension/state
POST /v8/management/plugins/session-quota-control/extension/assign
GET  /v8/management/plugins/session-quota-control/extension/usage
GET  /v8/management/plugins/session-quota-control/extension/usage/request?id=<lifecycle-id>
```

Assignment body: `{"id":"<pending-id>","auth_id":"<credential-id>"}`.
Immediate fallback body: `{"id":"<pending-id>","release":true}`.

Report query parameters: `start`/`end` (`YYYY-MM-DD`, inclusive, server time zone),
`bucket` (`hour`, `day`, `month`), `model`, `provider`, `credential`, `outcome`, `q`,
`page` and `page_size` (1–200, default 50). The default range is the last seven days.
The response includes `summary`, `buckets`, `models`, `credentials`, `keys`, and a
paginated `events` list; detail returns all admissions, selections and executions.
Filters select complete matching requests, retaining all their retry attempts.

The host adds an optional `session_affinity_gate` capability (`affinity.await`),
`host.context.wait` for cancellable pre-execution waits, `host.auth.quota` for
secret-free passive quota snapshots, and `ExtensionRoutes` confined to each
plugin's authenticated v8 namespace. `host.auth.list` accepts `include_runtime`
to include configuration credentials without exposing their secrets. Existing plugin contracts remain supported;
no new `/v0/management` endpoints are introduced. Payloads are not rewritten.

## Verification

```bash
go test ./examples/plugin/session-quota-control ./sdk/pluginabi ./sdk/pluginapi
go test ./sdk/cliproxy/auth -run TestSessionAdmission
go test ./internal/pluginhost -run 'TestContextWait|TestV8PluginExtensions'
go build -o test-output ./cmd/server
rm test-output
```

Tests use explicit clocks and deterministic synchronization for deadlines,
cancellation, native sticky routing, one-time calculations, model averages, historical
concurrency, fixed overrides and caps.
