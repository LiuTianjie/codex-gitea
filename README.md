<p align="center">
  <img src="docs/assets/logo.svg" width="96" height="96" alt="gitea-review-agent logo" />
</p>

<h1 align="center">gitea-review-agent</h1>

<p align="center">
  Self-hosted pull request review automation for Gitea.
</p>

<p align="center">
  <a href="https://liutianjie.github.io/gitea-review-agent/">Project site</a>
  ·
  <a href="#quick-start">Quick start</a>
  ·
  <a href="#configuration">Configuration</a>
</p>

gitea-review-agent receives Gitea pull request events through webhooks, prepares
a read-only checkout, runs a configured reviewer, and posts review findings back
to the PR. It uses Codex by default, with Codex provider/account selection
managed through cc-switch. It can also run Claude Code or a MiniMax-compatible
reviewer as separate review backends. MiniMax-compatible runs reuse the Claude
Code path and can be routed through cc-switch providers or an
Anthropic-compatible relay endpoint.

Reviews are stateful: follow-up pushes and `/review` comments can continue the
same reviewer session instead of starting from an empty context every time.

## Screenshots

<p align="center">
  <img src="docs/assets/console-analytics.png" alt="Analytics screen showing review success rate, findings, and trends" />
</p>

<p align="center">
  <img src="docs/assets/console-skill.png" alt="Project rules screen showing repository-scoped rule exports" />
</p>

## Features

- **Gitea PR review automation** — handles pull request, sync, and issue comment
  events from Gitea webhooks.
- **Pluggable reviewers** — Codex, Claude Code, and MiniMax-compatible reviewer
  backends can be configured independently.
- **cc-switch provider routing** — Codex uses cc-switch by default; the console
  can read Codex providers, model ids, and reasoning-effort values from
  `cc-switch.db`. Claude Code and MiniMax-compatible runs can also switch
  providers through cc-switch before each run.
- **Read-only checkouts** — reviewer processes inspect the diff and do not run
  repository code.
- **Incremental git cache** — repositories are mirrored under `/cache`, then
  checked out into deterministic worktrees for each job.
- **Admin console** — `/admin` shows review jobs, logs, runtime configuration,
  analytics, and project rule exports.
- **Configurable alert analysis** — receives the structured alerts already sent
  to Feishu, queries the matching Aliyun SLS raw logs, inspects a configured
  repository and Git history, and reports results through a Feishu webhook or
  live-updates one card through a Feishu app bot.
- **Project rules** — each repository can expose a downloadable `SKILL.md` at
  `/skills/<owner>/<repo>/SKILL.md`.
- **Readiness endpoint** — `/healthz` is a liveness check; `/readyz` reports DB,
  configuration, queue counters, and latest failure context.

## How it works

```text
Gitea webhook
  -> verify HMAC
  -> enqueue job in SQLite
  -> fetch cached mirror + prepare worktree
  -> run configured reviewer
  -> map findings to diff lines
  -> publish review through Gitea API
```

Alert analysis is a separate queue and does not change the PR review lifecycle:

```text
existing alert action
  -> keep sending the original Feishu alert
  -> POST the same alert context to /hooks/alert-analysis/{config_id}/{token}
  -> query SLS around the alert time
  -> prepare a read-only repository revision using the global Gitea token
  -> locate endpoints/tasks, code evidence, commits, and suggested contacts
  -> app bot: send once and update the same card for every phase
  -> custom webhook: send one card only when the task reaches a terminal state
```

## Reviewer backends

Codex is the default reviewer. Claude Code and MiniMax-compatible reviewers are
optional and keep their own reviewer identity, sessions, logs, and PR comments.

Codex authentication defaults to `ccswitch`. The `/admin` console is the primary
configuration surface: set the Codex Base URL, API Key, model, and reasoning
effort there. A cc-switch provider id is optional; use it only when you already
have a provider you want to pin. The console can also read candidates from
`/cc-switch/cc-switch.db`, including provider-level Codex config such as:

```toml
model = "gpt-5.5"
model_reasoning_effort = "xhigh"

[model_providers.relay]
base_url = "https://relay.example.com/v1"
```

In `ccswitch` mode, if a provider id is selected, the runner switches to that
provider and syncs its Codex config into the runtime `CODEX_HOME`. If the
provider field is empty, the runner first tries to match the console Base URL
to an existing cc-switch provider; when none matches, it builds the Codex
runtime config directly from the console Base URL. The app default reasoning
effort is `high` for review depth.

If model candidates are empty in `/admin`, fetch them once with cc-switch:

```bash
cc-switch --app codex provider fetch-models --base-url https://relay.example.com/v1 <provider-id>
```

MiniMax-compatible review runs through the Claude Code execution path. There are
two supported ways to route it:

- **cc-switch provider** — set `MINIMAX_PROVIDER_ID` to switch Claude Code to a
  configured provider before the MiniMax review run.
- **Direct relay endpoint** — set `MINIMAX_API_KEY` and `MINIMAX_BASE_URL` for an
  Anthropic-compatible MiniMax or relay endpoint.

Example:

```bash
MINIMAX_ENABLED=true
MINIMAX_PROVIDER_ID=minimaxreview
# or:
MINIMAX_API_KEY=...
MINIMAX_BASE_URL=https://relay.example.com
```

The job is still stored and posted as the `minimax` reviewer, so it can be
tracked separately from Codex and Claude Code.

## Production deploy (Luma)

The live service is Luma application `codex-gitea` at
`https://codex-bot.itool.tech`. Production updates are a local Docker/Buildx
build that Luma uploads to the Builder Registry and rolls out in the same
command. Do not publish GHCR `:latest` and then `luma deploy` that mutable tag.

From the commit you want live, on a machine that can reach the Builder Registry:

```bash
luma build local . --platform linux/amd64 --timeout 3000
```

`--platform linux/amd64` matches the pinned `lab` node. `luma.yaml` is
gitignored and holds runtime secrets plus the existing volume names; keep those
volume names unchanged so `/data`, `/cache`, `/work`, `/codex-home`,
`/claude-home`, and `/cc-switch` are preserved. GitHub Actions still builds a
GHCR image for CI smoke tests; that image is not the production rollout path.

A deploy is complete only after Luma reports a new stable `codex-gitea`
version whose image uses a `local-<build-id>` tag (not GHCR `latest`), and both
`/healthz` and `/readyz` return `ok`.

## Quick start

Local trial, not production:

```bash
docker compose up -d
```

Set `ADMIN_PASSWORD` (and `SECRET_KEY` before creating alert-analysis configs).
Then open `http://localhost:8080/admin`, finish the runtime configuration, and
add a Gitea webhook pointing to `http://<host>:8080/webhook`.

Persist these paths in production:

- `/data` — SQLite database
- `/cache` — bare git mirrors
- `/work` — temporary worktrees
- `/codex-home` — Codex config and sessions
- `/claude-home` — Claude Code state
- `/cc-switch` — cc-switch provider/proxy/account configuration

## Auth: cc-switch (default), authfile, or apikey

| Mode | Cost | Setup |
|------|------|-------|
| `ccswitch` (default) | depends on the configured relay/account | set Codex Base URL + API Key in `/admin`; optionally set `CODEX_CC_SWITCH_PROVIDER_ID` or save `codex_cc_switch_provider_id` to pin an existing cc-switch provider |
| `authfile` (legacy) | reuses your ChatGPT subscription, **no extra API billing** | run `codex login` locally and place `~/.codex/auth.json` in `/codex-home` |
| `apikey` | **separately billed** OpenAI Platform tokens | set `CODEX_API_KEY` + `CODEX_AUTH_MODE=apikey` |

In `ccswitch` mode the `/cc-switch` volume **must be writable** so cc-switch can
store providers, accounts, proxy state, and Codex live config. `CODEX_CC_SWITCH_PROVIDER_ID`
is optional; when it is set, the runner switches to that Codex provider before
each review. When it is empty, the runner uses the console Base URL as the
source of truth and does not require an existing provider. The admin console
reads Codex providers, model ids, and `model_reasoning_effort` values from
`cc-switch.db`; saved model/reasoning values are then passed to Codex as
explicit overrides.

## First-run checklist

1. **Codex**: in `/admin`, set `Codex Auth Mode=ccswitch`, Codex Base URL, API Key, model, and reasoning effort. Leave provider blank unless you intentionally want to pin an existing cc-switch provider.
2. **Gitea bot**: a Gitea user with a token scoped **repo read + PR write**; add it to private repos.
3. **Console password**: set `ADMIN_PASSWORD` (no password ⇒ `/admin` returns 503).
4. **Console config** (`/admin`): Gitea URL, bot token, webhook secret, Codex
   provider/model/reasoning effort, trigger keywords, repo allowlist. DB settings
   override env and apply without a restart.
5. **Gitea webhook** (per repo or org-level):
   - URL `http://<host>:8080/webhook`, Content-Type `application/json`
   - Secret = the webhook secret you set in the console
   - Events: **Pull Request** + **Pull Request Sync** + **Issue Comment**
6. **Deployment check**: `/readyz` should return `{"ok":true,...}` after the
   first boot. If it returns 503, inspect `config_warnings` first.

## Usage

- Open a PR or push commits → automatic review.
- Comment `/review <question>` on a PR → answered with the prior review's context.
- Open `/admin` → inspect jobs, cancel pending jobs, rerun failed jobs, update
  runtime config, generate analytics, and manage project rules.
- Open `/skills/<owner>/<repo>/SKILL.md` → download a generated project rule file
  without console authentication.

## Admin console

### Tasks

The task tab shows recent jobs, status counters, retryable pending work, canceled
and superseded jobs. Pending jobs can be canceled from the detail panel; worker
finishes are conditional, so an old worker cannot overwrite a canceled or
superseded final state.

### Analytics

Analytics reports are stored snapshots. Reports include:

- finding and success-rate trends from existing review/finding data
- severity/status/reviewer/developer distributions
- high/critical recent issues with Gitea line links
- repeated titles and multi-agent overlap scoped to the same PR

### Project rules

The Rules tab is project-scoped. It uses existing findings as evidence and can
produce a repository-specific `SKILL.md` file for future development and review.

Generation is asynchronous:

1. `POST /admin/api/skills/<owner>/<repo>/generate` returns a background `task_id`.
2. The UI polls `GET /admin/api/skills/<owner>/<repo>/generate/<task_id>`.
3. When the task finishes, the UI refreshes the version, content, download link,
   and copyable usage instruction.

Usage instruction format:

```text
请使用这个项目规则文件：https://<host>/skills/<owner>/<repo>/SKILL.md
```

### Alert analysis

The **Alert configs** tab creates independent, database-backed configurations.
Each configuration selects its own analysis branch/ref through `repository_ref`
(default `main`): for example, `dev` for test and `main` for production. Each
analysis fetches that ref again, using the latest commit for a branch or the
specified commit for a fixed SHA. An alert’s `deployment_sha` does not override
this choice. Repository connection tests use the same configured ref. The
resolved SHA is retained in task events and code evidence. Missing deployment metadata is not itself an evidence gap; version
verification is requested only when logs visibly contradict the current code.
Each configuration includes a repository URL, SLS endpoint/project and one
or more comma-separated logstores and credentials, one of two Feishu delivery
modes, optional model/prompt overrides, and a duplicate-alert throttle. App-bot
mode stores an App ID, encrypted App Secret, and target Chat ID; webhook mode
stores an encrypted custom-bot webhook. Repository checkout reuses the global
`GITEA_TOKEN`; there is no second Gitea token in an alert configuration.
Each alert configuration also has its own analysis concurrency (default `2`,
range `1-16`). The scheduler enforces that limit independently, so a busy test
configuration does not consume the production configuration's own slots; the
service-wide safety cap is 16 simultaneous alert analyses.

An alert configuration can ignore exact error codes before task creation. The
console accepts comma-, whitespace-, semicolon-, or newline-separated values,
for example `4290, 5001`. Matching is case-insensitive and exact, so ignoring
`4290` does not ignore `429` or `42900`. A filtered delivery returns HTTP 202
with `filtered: true`, creates no analysis task, consumes no concurrency slot,
and sends no Feishu analysis card.

Each alert configuration can also maintain its own Git-author-to-Feishu-user
mapping. Enter one record per line in the console using this format:

```text
znc,Starslayerx | 张宁池 | ou_xxx
Lin | 陈惠琳 | ou_yyy
```

Aliases are matched case-insensitively against suspect commit authors, commit
email prefixes, and the model's suggested contacts. Only valid `ou_...` IDs are
rendered as real Feishu mentions. A successful final card mentions at most three
matched users; progress, failed, canceled, and duplicate-alert cards never
mention anyone. The mapping is copied into each task snapshot, so later config
edits do not change the audit trail for an already-created analysis.

Creating or rotating a configuration returns its full receiver URL once. Add a
second HTTP action after the existing Feishu alert action and POST structured
JSON to that URL. The original Feishu action stays unchanged. A minimal payload
is:

```json
{
  "delivery_id": "unique-delivery-id",
  "alert_id": "sls-alert-id",
  "alert_time": "2026-08-27T15:30:00+08:00",
  "environment": "PROD",
  "service": "serverx",
  "rule": "server-error-rate",
  "title": "POST /api/example failed",
  "severity": "high",
  "method": "POST",
  "endpoint": "/api/example",
  "event_id": "event-id-if-available",
  "trace_id": "trace-id-if-available",
  "error_code": "500",
  "error_message": "short error text",
  "deployment_sha": "optional-historical-context-only",
  "detail_url": "optional-alert-detail-url"
}
```

`delivery_id` is the idempotency key within one configuration. If omitted, the
receiver falls back to `X-Alert-Delivery-ID`, then `alert_id`, then a payload
hash. SLS lookup prefers `event_id` or `trace_id`, falls back to `endpoint`, and
queries the configured time window around `alert_time` (RFC3339, Unix seconds,
or Unix milliseconds). Additional JSON fields are retained as redacted evidence;
fields whose names contain token, secret, password, access_key, or authorization
are removed before persistence.

The receiver uses the unguessable token in its URL instead of a separate
signature protocol. Disable a configuration to reject new alerts without losing
history; deleting it removes its credentials and receiver token while keeping
existing task snapshots. The console shows live phase events and supports
cancel and retry. Final results include an AI-assessed severity, its reasoning,
and the estimated impact scope; these are kept separate from the source alert's
severity and fall back to an explicit evidence-gap result when the available
logs cannot support a reliable assessment.

Duplicate throttling is consecutive and configurable. By default, only the
first alert for the same method, endpoint, error code, and error message runs an
analysis. Later matching alerts are stored as `suppressed` and remain visible
only in the console; they never send another group message. The same
fingerprint stays suppressed until a different error arrives; set a non-zero
cooldown in the console if periodic re-analysis is desired. Replayed deliveries
with the same `delivery_id` remain idempotent and do not create another task.

Alert analysis uses a separate bounded Git cache instead of cloning the full
repository for every task. It shallow-fetches only the configured branch or
deployment SHA (default depth 200), keeps at most three recently used bare
repositories and never caches worktrees. Each task gets a temporary worktree
which is deleted on completion; a janitor removes crash leftovers after one
hour. The cache is also bounded by 5 GiB, seven days of idleness, and a 1 GiB
free-disk watermark. All limits are editable in the console under **告警 Git 缓存**
and apply to later fetch/cleanup passes without restarting the service. Existing
legacy full incident mirrors are replaced on first use. PR-review mirrors keep
their existing behavior and are not counted in these alert-analysis limits.

Feishu custom webhooks do not return a message id, so webhook mode sends only
one terminal card and never posts progress cards. Feishu app-bot mode obtains a
tenant access token, sends the initial shared card through the IM API, persists
the returned `message_id`, and patches that same card for later phases. The app
must have bot capability, be a member of the target chat, and hold one of the
documented send/update message permissions. Duplicate alerts remain silent in
both delivery modes.

## GitHub Pages

`.github/workflows/pages.yml` publishes the static site in `docs/` to GitHub
Pages on every push to `main`. In the repository settings, set Pages source to
**GitHub Actions** if it is not already enabled.

Default Pages URL:

```text
https://liutianjie.github.io/gitea-review-agent/
```

## Configuration

Env vars (all optional except `ADMIN_PASSWORD`; the console can set the rest):

| Var | Default | Notes |
|-----|---------|-------|
| `ADMIN_PASSWORD` | — | required; protects `/admin` |
| `SECRET_KEY` | — | required before saving alert configurations; use a stable random secret to AES-GCM encrypt SLS/Feishu credentials in SQLite |
| `PUBLIC_URL` | — | optional public service base URL used by Feishu cards to link back to the alert task in `/admin` |
| `GITEA_URL` / `GITEA_TOKEN` | — | bot account |
| `GITEA_TIMEOUT` | `90s` | per Gitea API request; also configurable in console |
| `WEBHOOK_SECRET` | — | HMAC-SHA256 verification |
| `MODEL` | `gpt-5-codex` | codex model |
| `CODEX_BASE_URL` | — | optional Codex relay/provider base URL; also configurable in console |
| `CODEX_AUTH_MODE` | `ccswitch` | or legacy `authfile`, or `apikey` |
| `CODEX_CC_SWITCH_PROVIDER_ID` | — | optional cc-switch Codex app provider id switched before Codex runs |
| `CODEX_API_KEY` | — | apikey mode only (separately billed) |
| `CODEX_SANDBOX_MODE` | `read-only` | set `danger-full-access` only when the container blocks Codex's read-only sandbox |
| `CLAUDE_ENABLED` | `false` | enable the Claude reviewer |
| `CLAUDE_MODEL` | `sonnet` | Claude model/alias passed to Claude Code |
| `CLAUDE_API_KEY` | — | optional Anthropic or relay key; configurable in console |
| `CLAUDE_BASE_URL` | — | optional Anthropic-compatible relay URL; configurable in console |
| `CLAUDE_MAX_BUDGET_USD` | `0.3` | per Claude Code run budget cap; set `0` to disable |
| `CC_SWITCH_CONFIG_DIR` | `/cc-switch` | cc-switch provider/proxy config directory |
| `CC_SWITCH_PROVIDER_ID` | — | optional provider id to switch before Claude runs |
| `MINIMAX_ENABLED` | `false` | enable the MiniMax reviewer via Claude Code |
| `MINIMAX_PROVIDER_ID` | — | optional cc-switch Claude app provider id used before MiniMax review runs |
| `MINIMAX_API_KEY` | — | optional MiniMax/relay API key passed to Claude Code |
| `MINIMAX_BASE_URL` | — | optional MiniMax/relay Anthropic-compatible base URL |
| `MINIMAX_MODEL` | — | optional `claude --model` override; leave empty to use provider/relay defaults |
| `MINIMAX_MAX_BUDGET_USD` | `0.3` | per MiniMax/Claude Code run budget cap; set `0` to disable |
| `CONCURRENCY` | `5` | worker count |
| `TRIGGER_KEYWORDS` | `/review,@review` | comma-separated |
| `REPO_ALLOWLIST` | — | comma-separated `owner/repo`; empty = all |
| `TIMEOUT` | `30m` | per codex run |
| `ANALYSIS_GIT_FETCH_DEPTH` | `200` | commits fetched for the configured alert-analysis branch/SHA |
| `ANALYSIS_CACHE_MAX_REPOSITORIES` | `3` | maximum recently used alert repository caches |
| `ANALYSIS_CACHE_MAX_MB` | `5120` | total alert repository cache limit in MiB |
| `ANALYSIS_CACHE_MAX_IDLE` | `168h` | evict an unused alert repository after this duration |
| `ANALYSIS_WORKTREE_TTL` | `1h` | remove worktrees left behind by interrupted tasks |
| `ANALYSIS_CACHE_CLEANUP_INTERVAL` | `10m` | background janitor interval |
| `ANALYSIS_MIN_FREE_MB` | `1024` | reject/evict alert caches below this free-space watermark; `0` disables it |

## Security

- The `/admin` console can change tokens and upload credentials — **do not expose
  it publicly**. Keep it on a private network or behind a reverse-proxy auth layer.
- PR content is untrusted: codex runs read-only and its output is treated as data.
  Gitea/OpenAI tokens are never injected into the worktree environment.

## Development

```bash
go build ./...
go test ./...
```

The admin console is a Vite/React app embedded into the Go binary. The
Dockerfile builds it automatically before compiling the service. Production
rollout is `luma build local` from this checkout, not a GHCR pull. For local
`go build` / `go test`, rebuild the console after changing UI code:

```bash
cd internal/console/frontend
npm install
npm run build
```

Module layout: `internal/{webhook,queue,review,gitcache,codex,gitea,store,config,console}`,
wired in `cmd/codex-gitea/main.go`. Interfaces live in `internal/model/types.go`.
