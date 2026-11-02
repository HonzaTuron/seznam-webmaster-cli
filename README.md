# szwm: Seznam Webmaster CLI

[![CI](https://github.com/HonzaTuron/seznam-webmaster-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/HonzaTuron/seznam-webmaster-cli/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**English** | [Čeština](README.cs.md)

An agent-first command-line tool and MCP server for the [Seznam.cz Webmaster](https://reporter.seznam.cz/wm/) API. Check how Czech search engine Seznam sees your site, audit indexation and request reindexing, from a terminal, a cron job or an AI agent.

> This is an **unofficial** project. It is not made or endorsed by Seznam.cz, a.s.

## Why another API wrapper?

Most CLIs are built for people and later parsed by machines. `szwm` is designed the other way round, around what breaks when an agent drives a tool:

| Problem for agents | What szwm does |
|---|---|
| Output changes between runs | One stable JSON envelope, versioned by `schema_version` |
| Failures can't be told apart | 19 stable error codes, 13 exit codes, plus `retryable` and a `hint` |
| Tools hang waiting for input | Never prompts: bulk writes fail fast until you pass `--yes` |
| Rate limits are hit by loops of separate processes | Limits are enforced **across processes**, per API key |
| Daily quotas run out mid-batch | Batches are cut to the remaining quota and the rest is reported per URL |
| Responses flood the context window | `--fields`, `--counts-only`, `--brief`, default URL limits, NDJSON streaming |
| Secrets leak through logs | No `--api-key` flag. The key is stripped from errors and traces |
| Crawled pages can carry prompt injection | `meta.untrusted_fields` names every third-party field |
| Docs drift from the binary | `szwm schema` is generated from the command tree |
| The upstream API changes silently | `szwm doctor` compares the live spec with the built-in snapshot |

## Installation

Download a binary for Linux, macOS or Windows from [Releases](https://github.com/HonzaTuron/seznam-webmaster-cli/releases), or build it with Go 1.26 or newer:

```bash
go install github.com/HonzaTuron/seznam-webmaster-cli/cmd/szwm@latest
```

`go install` puts the binary in `$(go env GOPATH)/bin`, usually `~/go/bin`. If that directory isn't on your `PATH`, add it or call the binary by its full path.

## Quick start

1. Pick your site in Webmaster and copy its key from **[https://reporter.seznam.cz/wm/web/api-klic](https://reporter.seznam.cz/wm/web/api-klic)**. Each site has its own key.
2. Save it. It is read from stdin, so it stays out of your shell history. The site is detected from the key:

   ```bash
   printf %s "$KEY" | szwm auth login --profile shop
   ```

3. Check that everything works, then look at the site:

   ```bash
   szwm doctor
   szwm report
   ```

For scripts and CI, skip the profile and set `SZWM_API_KEY`.

## Commands

| Command | What it does | Effect | Key |
|---|---|---|---|
| `report` | Health summary with stable finding codes. **Start here** | read | yes |
| `web` | Counts, URL samples and history | read | yes |
| `documents` | Counts per category with a random URL sample | read | yes |
| `history` | Daily counts over time | read | yes |
| `document [url...]` | How Seznam sees specific pages; handles batches | read | yes |
| `reindex [url...]` | Request reindexing | **write** | yes |
| `database-info` (`db`) | When the data was last released | read | no |
| `quota` | Local estimate of today's reindex quota | local | no |
| `api <METHOD> <path>` | Direct API call with auth, limits and retries handled | varies | varies |
| `auth login\|status\|list\|use\|logout` | Manage per-site profiles | local | |
| `doctor` | Diagnose setup, key, connectivity and spec drift | read | no |
| `schema [command]` | Machine-readable contract | local | no |
| `guide` | Agent guide; `--install-skill` installs it | local | no |
| `mcp` | Run an MCP server on stdio | read/write | |
| `mcp install` | Register this binary in Claude Code by absolute path | local | no |
| `version`, `completion` | Version info and shell completion | local | no |

Global flags include `--profile`, `-o/--output`, `--fields`, `--raw`, `--pretty`, `--read-only`, `--timeout`, `--deadline`, `--retries` and `--verbose`. See `szwm schema --fields global_flags`.

## Recipes

```bash
# Is anything wrong? Exit 12 at warning or worse, which suits cron.
szwm report --window 30 --fail-on warning

# Which pages have errors? This is a random sample, not a full list.
szwm documents --category error --limit 50

# Why is this page not on Seznam?
szwm document https://example.com/page --brief

# Page counts for a spreadsheet
szwm history --last 90d -o csv > history.csv

# Audit a sitemap. Results stream while they arrive, at about 100 URLs a minute.
szwm document --sitemap https://example.com/sitemap.xml --brief -o ndjson > audit.ndjson

# Reindex only what is missing: preview first, then submit.
jq -c 'select(.type == "item" and .state == "not_indexed")' audit.ndjson > todo.ndjson
szwm reindex --file todo.ndjson --dry-run --fields summary,quota
szwm reindex --file todo.ndjson --yes --deadline 10m

# Call an endpoint that has no dedicated command
szwm api GET /web/document --param url=https://example.com/
```

## For AI agents

### The contract

- Output is JSON whenever stdout is not a terminal. Change it with `-o` or `SZWM_OUTPUT`.
- In `json` mode, stdout holds **exactly one document**, on success and on failure.
- In `ndjson` mode, each item is one line with `"type":"item"`, and the last line has `"type":"summary"`. Items stream, so a killed run keeps what it finished.
- On failure, stderr also gets a one-line error, so it stays visible when stdout is piped.
- Exit code 0 means `ok: true`, always.

```json
{
  "schema_version": "1",
  "ok": false,
  "command": "web",
  "error": {
    "code": "auth_invalid",
    "message": "Seznam rejected the API key: API key does not exist.",
    "hint": "Copy the site's current key from https://reporter.seznam.cz/wm/web/api-klic, then run `szwm auth login`.",
    "retryable": false,
    "http_status": 401,
    "exit_code": 3
  },
  "meta": { "szwm_version": "0.2.0", "duration_ms": 127, "requests": 1, "retries": 0 }
}
```

`ok: false` can still carry `data`: a partial batch tells you exactly which URLs finished. `meta` may also include `truncated`, `hints`, `warnings` and `untrusted_fields`.

### Exit codes

| Exit | Error codes | Meaning | Retry? |
|---|---|---|---|
| 0 | | Success | |
| 1 | `internal`, `api_error`, `doctor_failed` | Unexpected failure, or a `doctor` check failed | no |
| 2 | `invalid_argument`, `confirmation_required` | Fix the command | no |
| 3 | `auth_missing`, `auth_invalid` | Missing or rejected key | no |
| 4 | `forbidden` | URL outside this site, or no write rights | no |
| 5 | `not_found` | Seznam has no data for it | no |
| 6 | `no_data_yet` | Valid request, data not ready | later |
| 7 | `rate_limited` | Wait `retry_after_seconds` | yes |
| 8 | `service_unavailable`, `network_error`, `timeout`, `bad_response` | Transient failure | yes |
| 9 | `partial_failure` | Batch partly finished | failed items |
| 10 | `quota_exhausted` | Daily reindex quota used up | tomorrow |
| 11 | `read_only` | Read-only mode blocked a write | no |
| 12 | `findings_threshold` | `report --fail-on` matched | no |

### Self-description

```bash
szwm schema                                    # the whole contract
szwm schema reindex                            # one command: flags, effect, API call, examples
szwm schema --fields commands.command,commands.effect
szwm guide                                     # usage guide written for agents
```

### Saving tokens

```bash
szwm web --counts-only                         # no URL samples
szwm documents --category error --limit 20     # one category, few URLs
szwm document --stdin --brief --fields summary < urls.txt
szwm doctor --fields checks.name,checks.status
```

`--fields` takes dotted paths and maps over arrays, so `items.url` picks `url` from every item. URL lists default to 100 per category, and `meta.truncated` tells you when something was cut.

### Skill file

The agent guide is built into the binary and kept at [`skills/seznam-webmaster/SKILL.md`](skills/seznam-webmaster/SKILL.md):

```bash
szwm guide --install-skill ~/.claude/skills
```

### MCP server

`szwm mcp` runs an MCP server on stdio. It is not a Seznam product: it sits on top of their REST API, with the same rate limiting, quota tracking and safety checks as the CLI.

Claude Code:

```bash
szwm mcp install
claude mcp list
```

`szwm mcp install` registers the binary by its absolute path, so it works even when `szwm` isn't on `PATH`. If it isn't, run it as `~/go/bin/szwm mcp install`. It registers for all your projects; use `--scope local` or `--scope project` to narrow that. Restart any running Claude Code session afterwards.

The server is read-only by default, which means the write tool isn't registered at all and a client can't see it. To allow reindexing:

```bash
szwm mcp install --allow-writes --force
```

Keys stay in szwm's own config and are never written to the client's, so run `szwm auth login` for each site first. Without a key the server still connects, but its tools fail with `auth_missing`.

Other clients: `szwm mcp install --print` outputs a ready-made config with the absolute path:

```json
{
  "mcpServers": {
    "seznam-webmaster": {
      "command": "/absolute/path/to/szwm",
      "args": ["mcp", "--read-only"]
    }
  }
}
```

Adding it by hand with `claude mcp add seznam-webmaster -- szwm mcp --read-only` works only if `szwm` is on the `PATH` Claude Code sees.

| Tool | What it does | Writes |
|---|---|---|
| `health_report` | Site health with findings. Start here | no |
| `database_info` | Data release time | no |
| `site_overview` | Counts and history; URLs only on request | no |
| `list_documents` | Counts with a random URL sample | no |
| `documents_history` | Daily counts | no |
| `get_document` | One page | no |
| `check_documents` | Up to 100 pages | no |
| `quota_status` | Local reindex quota estimate | no |
| `list_profiles` | Configured sites, without keys | no |
| `reindex_urls` | Up to 100 pages, with `dry_run` and `confirm_bulk` | **yes** |

## Sites and profiles

Every site has its own key, shown at [https://reporter.seznam.cz/wm/web/api-klic](https://reporter.seznam.cz/wm/web/api-klic) for the selected site. The API has no site parameter, because the key selects the site, so keep one profile per site:

```bash
printf %s "$KEY_A" | szwm auth login --profile shop --default
printf %s "$KEY_B" | szwm auth login --profile blog
szwm report --profile blog
szwm report --profile example.org    # the detected site works too
```

The key is resolved in this order, first match wins:

1. `--api-key-file` (`-` means stdin)
2. `--profile`
3. `SZWM_API_KEY`, then `SEZNAM_WEBMASTER_API_KEY`
4. `SZWM_PROFILE`
5. The default profile, or the only profile

## Rate limits and quota

Seznam publishes these limits, checked per user and per site:

- 5 requests per second
- 100 requests per minute
- 500 reindex requests per day

`szwm` paces requests using a small state file per key, so a shell loop that starts a new process per URL stays within the limits too. If the state file can't be written, it falls back to limiting within the process and the API call still goes ahead.

Transient failures (429, 502, 503, 504, network errors, timeouts) are retried with exponential backoff and jitter, respecting `Retry-After` up to 60 seconds. Longer waits return `rate_limited` with `retry_after_seconds`.

The API has no quota endpoint, so `szwm quota` is an **estimate** from this machine's ledger. Requests made elsewhere are not counted. Seznam doesn't say when a day starts, so `szwm` assumes midnight Europe/Prague. Use `--ignore-quota` if you know better.

## Health report

`szwm report` makes two API calls and returns totals, ratios, a trend and findings with stable codes. The overall `severity` is the worst finding.

| Code | Severity | Triggered when |
|---|---|---|
| `nothing_indexed` | critical | Pages were downloaded, none are indexed |
| `low_index_ratio` | warning | Under 50% of downloaded pages are indexed |
| `high_error_ratio` | warning / critical | Errors are at least 10% / 25% of crawled pages |
| `index_drop` | warning / critical | Indexed pages fell at least 10% / 30% over the window |
| `error_rise` | warning / critical | Errors rose by at least 10 pages and 20% / 100%, or from zero |
| `stale_history` | warning | The newest history row is over 4 days old |
| `stale_release` | warning | The data release is over 3 days old |
| `no_history` | info | Not enough history for a trend |

These thresholds are heuristics of this tool, not Seznam rules.

## Security

- **No key on the command line.** Flags leak into process lists and logs, so there is no `--api-key`.
- **The key must travel in the URL**, because that is how the API works. `szwm` strips it from error messages and `--verbose` traces.
- **Stored keys** live in a `0600` config file in plain text. `doctor` fails if the file is readable by others.
- **Saved Webmaster pages contain keys.** A page saved from the browser includes the API keys of all your sites in its page data, so don't share or commit those files.
- **Local state** contains only a fingerprint of the key, never the key.
- **Redirects are never followed**, since following one could send the key elsewhere.
- **Base URL is restricted** to `https://reporter.seznam.cz` and loopback, so a manipulated agent can't send the key to another host.
- **Sitemap requests use a separate HTTP client** that has no access to the key.
- **Crawled content is untrusted.** `title`, `meta`, `openGraphData` and `responseHeaders` come from web pages and may contain text aimed at an agent. `meta.untrusted_fields` lists them.
- **`--read-only`** or `SZWM_READ_ONLY=1` blocks all writes, including through `szwm api`.

## Things to know about the API

Verified against the live service unless noted. Where the API and its spec disagree, `szwm` follows the API and passes unknown fields through:

- The spec's `basePath` is `/wm-api/wm-api`, but the working prefix is `/wm-api`.
- Errors carry `detail`, a string or a list of strings, while the published `ProblemResult` schema says `description`. `szwm` reads all of these.
- `doc_count` is a normal `{count, urls}` category, although the published models omit it.
- `/web` and `/web/document` return an undocumented `webserver` field in reversed form, such as `com.example.!443`. `szwm` turns it into `example.com`.
- Items in `responseHeaders` are `{name, content}`, not `{name, value}` as published.
- A URL outside the key's site returns 404 like an unknown page, not the documented 403.
- `downloadTimestamp` and `indexTimestamp` are Unix seconds. `szwm` adds them as `downloaded_at` and `indexed_at`.
- `urls` is a **random sample** of at most 1,000 per category, and it can be shorter than `count` even on small sites. Use `count` for totals and `--sitemap` for an inventory.
- On the site tested, the `content` sample didn't overlap the `index` sample.
- Data changes only with a release (`szwm database-info`). A reindex request shows up after a later release, so polling within the same one is pointless.
- Replicas report release times a fraction of a second apart. Compare releases by date.
- History rows still include the deprecated `downloaded` category. Prefer `doc_count`.
- Not documented, and not seen in testing: the shape of a 429 response and the time the daily quota resets.

## Testing status

The test suite runs the full CLI and MCP server against a local fake API, with the race detector on. Mutation checks confirmed that the tests catch removal of the key redaction, redirect protection, base URL restriction, read-only mode, quota cut-off and bulk confirmation.

It has also been run against the live service with a real site key: every read command, batches, sitemap input, one real reindex request, the MCP tools and registration in Claude Code. Rate limiting held at 5 requests per second with no retries.

Not seen live, so based on the published spec only: a 403 from a key without write rights, a 204 for a site without data, and any 429 response. Please report anything that differs.

## Configuration

| Variable | Purpose |
|---|---|
| `SZWM_API_KEY`, `SEZNAM_WEBMASTER_API_KEY` | API key |
| `SZWM_PROFILE` | Default profile or site |
| `SZWM_OUTPUT` | Default output format |
| `SZWM_READ_ONLY` | `1` blocks all writes |
| `SZWM_BASE_URL` | API base URL: official host or loopback |
| `SZWM_CONFIG_DIR` | Config directory |
| `SZWM_STATE_DIR` | Rate-limit and quota state directory |
| `SZWM_INSECURE_ALLOW_ANY_BASE_URL` | `1` allows any base URL, which sends the key to that host |

## Development

```bash
make test    # go test -race ./...
make lint    # gofmt check and go vet
make build   # ./bin/szwm
```

`docs/swagger.snapshot.json` is the API contract the tool was built against. Agents working on this repository should read [AGENTS.md](AGENTS.md).

## License

[MIT](LICENSE). Seznam and Seznam.cz are trademarks of Seznam.cz, a.s., used here only to describe what this tool works with.
