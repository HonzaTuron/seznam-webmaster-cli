# Working on szwm

This file is for agents changing this repository. To **use** the tool, read [`skills/seznam-webmaster/SKILL.md`](skills/seznam-webmaster/SKILL.md) or run `szwm guide`.

## Layout

| Path | Purpose |
|---|---|
| `cmd/szwm` | Entry point |
| `internal/cli` | Cobra commands, envelope handling, `schema`, `doctor` |
| `internal/mcpserver` | MCP tools on top of `wm.Service` |
| `internal/wm` | Application logic shared by the CLI and MCP |
| `internal/api` | HTTP client: auth, retries, redaction |
| `internal/ratelimit` | Cross-process limiter and quota ledger |
| `internal/output` | JSON, NDJSON, table and CSV rendering |
| `internal/apperr` | Error and exit code catalog |
| `internal/config` | Profiles and key resolution |
| `internal/sitemap` | Sitemap reader, with no access to the key |
| `docs/swagger.snapshot.json` | API contract used by `doctor` and `schema` |

## Rules

- Put behaviour in `internal/wm`, so the CLI and MCP stay identical. Commands and tools only parse input and present output.
- Error codes, exit codes and envelope fields are public API. Add to them freely, and bump `output.SchemaVersion` before changing the meaning of anything that exists.
- New commands need the `meta(...)` annotations: `TestSchemaIsCompleteAndAccurate` fails without them.
- The key is part of every request URL. Never log or return a URL without `Redact`, and never wrap a transport error without unwrapping `url.Error` first.
- Never add a flag that accepts the key, a prompt in a code path agents reach, or a retry that bypasses the limiter.
- Decode API responses generically. The API returns fields its schema omits, and they must pass through.
- Fields copied from crawled pages must be listed in `Result.Untrusted`.
- Update `README.md`, `README.cs.md` and `SKILL.md` together when behaviour changes.

## Before you finish

```bash
make lint test
```

If `szwm doctor` reports `spec_drift`, refresh the snapshot from `https://reporter.seznam.cz/wm-api/swagger.json` and adapt the code.
