# mcpq

`mcpq` is a read-only CLI that calls an MCP (Model Context Protocol) server and returns only the parts of the response you asked for, filtered with jq.

It is built for agent/LLM workflows, where pasting raw MCP responses into the context is expensive and error-prone. `mcpq` is **fail-closed**: when anything is unexpected — a changed response shape, a truncated page, an expired snapshot, a schema that no longer matches the allowlist — it stops with a non-zero exit code instead of returning data that merely *looks* like a valid empty result.

Key properties:

- **Allowlist, deny by default.** Only servers, tools, and exact argument values listed in the config file may be called. Tool input schemas are pinned by SHA-256 hash; if a server's schema changes, calls stop.
- **Mandatory shape assertion.** Every call must state what shape it expects (`--expect`). A jq mistake that evaluates to `null` is an error, not "no results".
- **Completeness is explicit.** Results carry `complete: true / false / "unknown"`. Incomplete results exit with code 4 and only show data when you pass `--allow-partial`.
- **Explicit snapshots, no implicit cache.** `call` always fetches fresh data and stores the raw response. Re-filtering stored data requires naming it (`query <raw_id>`), and stale snapshots are refused.
- **One JSON envelope.** Stdout is always a single JSON object, `{"meta": {...}, "data": [...]}`, so metadata can never be stripped away from the data it describes.
- **Integers keep their digits.** JSON numbers are never round-tripped through float64; IDs and timestamps larger than 2^53 survive intact.

## Install

```sh
go install github.com/nyushi/mcpq@latest
```

Or build from source:

```sh
git clone https://github.com/nyushi/mcpq
cd mcpq
go build -o mcpq .
```

Requires Go 1.23 or later. Linux is assumed for the file-permission and ownership checks described below.

## Configuration

`mcpq` reads a single config file at a **fixed path** — `/etc/mcpq/config.json`. The path cannot be changed by a flag or an environment variable; this is deliberate, so that the agent using the tool cannot widen its own permissions. The file must be a regular file owned by you or an administrator, must not be a symlink, and must not be writable by group or others, or `mcpq` refuses to start (exit code 5).

The config contains **names, not secrets**: endpoints and tokens are referenced by environment variable name and read from the environment on every invocation. Values never appear in the config, in logs, or in output.

```json
{
  "raw_dir": "/example/mcpq/raw",
  "deadline": "30s",
  "max_age": "15m",
  "shadow_jq": ".",
  "servers": {
    "example-server": {
      "endpoint_env": "EXAMPLE_ENDPOINT",
      "token_env": "EXAMPLE_TOKEN",
      "tools": {
        "example_read": {
          "schema_hash": "EXAMPLE-NOT-A-VALID-HASH",
          "args": {"limit": [2]},
          "required": ["limit"],
          "page": {
            "items": "items", "next": "next",
            "limit_arg": "limit", "cursor_arg": "cursor", "max_pages": 20
          }
        }
      }
    }
  }
}
```

(This is `config.example.json` in this repository — fictional, and intentionally not usable as-is.)

- `servers.<name>.tools.<tool>.args` enumerates the **only argument values allowed**; unknown keys are rejected. `required` lists arguments that must be present.
- `schema_hash` is the SHA-256 of the tool's `{name, inputSchema}` as canonical JSON, captured when you approve the tool. `mcpq` never registers or updates it automatically.
- `page` (optional) declares how pagination works for a tool: where the items and the next-page cursor live (dot-separated paths), which arguments carry the limit and cursor, and the page cap. Only tools with a `page` declaration — or another explicit completeness proof in the response — can ever report `complete: true`; all others report `"unknown"`.
- `max_age` caps how old a snapshot may be when re-queried (default `15m`, hard maximum `60m`).
- `deadline` is the overall time budget per invocation (default `30s`, maximum `5m`).
- `raw_dir` is where snapshots and diff logs are stored (see below).
- `shadow_jq` is the jq expression `compare` applies to both sides before comparing (default `.`, exact comparison).

Set `MCPQ_DISABLE=1` to disable `mcpq` entirely; every command then exits with code 5, and callers should fall back to their pre-existing, already-authorized procedure.

## Usage

```sh
# Fetch fresh data and filter it. --expect is required.
mcpq call example-server example_read \
  --expect 'type == "array"' \
  --args '{"limit": 2}' \
  --jq '.[] | {id, name}'

# Re-filter a stored snapshot without contacting the server.
# --server and --tool are required and must match the stored call.
mcpq query 0123abcd... --server example-server --tool example_read \
  --expect 'type == "array"' --jq '.[0]'

# List the tools the config allows for a server (also verifies schemas).
mcpq tools example-server

# Compare two stored snapshots (shadow/differential checking).
mcpq compare example-server example_read --raw-id <id-a> --baseline-id <id-b>
```

Flags:

| Flag | Commands | Meaning |
|---|---|---|
| `--expect EXPR` | `call`, `query` | Required. jq expression evaluated against the (normalized) tool result; it must produce exactly one `true`, otherwise exit code 2. |
| `--args JSON` | `call` | Tool arguments. Must match the allowlist exactly. |
| `--jq EXPR` | `call`, `query` | jq filter applied to the result. The output stream becomes `data`. A `null` result is an error (code 2). |
| `--max-bytes N` | `call`, `query` | Cap on the filtered output. Whole results that don't fit are dropped and the result is marked incomplete (code 4). |
| `--max-age DURATION` | `query` | Refuse snapshots older than this. May not exceed the configured cap (code 1 if it does). |
| `--allow-partial` | `call`, `query` | Show `data` even when the result is incomplete (exit code stays 4). |

When scripting, use `set -o pipefail` and do not discard exit codes with `|| true` or `| head` — a non-zero code is the tool's main safety signal.

## Output

Stdout is one JSON object:

```json
{"meta": {"server": "example-server", "tool": "example_read",
          "fetched_at": "2026-01-01T00:00:00Z", "complete": true,
          "matched": 3, "raw_id": "…", "code": 0},
 "data": [ … ]}
```

`meta` always includes the server, tool, args, fetch time, response size, call counts (`tool_calls`, `http_requests`, `response_bytes`), `matched` (number of jq results), `complete`, and — for stored responses — `raw_id`, the raw file path, and its SHA-256. When `matched` is 0, `meta` also carries the top-level keys of the raw response to help tell "empty" apart from "wrong shape". On any non-zero exit, `data` is omitted (except code 4 with `--allow-partial`).

The jq input is the tool result's `structuredContent` when present; otherwise the `content` text blocks — parsed as JSON when possible, or passed as an array of strings when they are not JSON.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Usage error: bad syntax, invalid expression, `--max-age` over the configured cap |
| 2 | Fetch/shape/storage/comparison failure: RPC error, `isError`, broken stream, failed `--expect`, corrupt or mismatched snapshot |
| 3 | Authentication failure (HTTP 401/403, missing token). `mcpq` never tries to recover credentials |
| 4 | Incomplete result (`complete` is `false` or `"unknown"`) |
| 5 | Not allowed: config or permission problem, schema mismatch, or `MCPQ_DISABLE` is set |
| 6 | Snapshot is stale, or its timestamps are inconsistent |

When several apply, the highest priority wins: `1 > 5 > 3 > 2 > 6 > 4`. Codes 1 and 5 are decided before any network traffic (a schema mismatch is detected right after `tools/list`, before any tool call).

## Snapshots

Every successful or incomplete `call` stores its raw response under `raw_dir`:

- The directory must be owned by you and have mode `0700`; files are `0600`, published atomically, and symlinks are refused.
- A snapshot records the server, tool, args, fetch time, completeness, and integrity hashes alongside the response pages.
- `query` re-checks all of it: the stored server/tool must match `--server`/`--tool`, hashes must verify, the file's mtime must agree with the recorded fetch time, and the snapshot must be within the max-age cap. `query` also re-checks the *current* allowlist and schema before reading.
- Snapshots and diff logs are deleted 24 hours after creation, at the start of the next invocation. There is no background daemon.

`compare` takes two snapshots of the same server/tool/args, checks freshness and completeness on both sides, compares them (exactly, or through `shadow_jq`), and writes a hash-only diff log under `raw_dir` — timestamps, both sides' hashes, the comparison policy hash, and the verdict. Response bodies are never copied into the log.

## Safety model

The threat model is **mistakes, not malice**: `mcpq` protects against an agent (or a human) accidentally trusting a wrong, stale, or partial result, and against bugs in the tool itself. Hashes and file permissions exist to detect corruption and mix-ups between snapshots. It is *not* a security boundary against a malicious actor running as the same OS user, and it does not replace server-side authorization — use read-only credentials on the server side as well.

Data returned by a server is untrusted input: treat `data` as external content, not as instructions.

## Scope and status

- Protocol: MCP Streamable HTTP, version `2025-06-18`, POST with JSON or SSE responses. Plain HTTP is accepted for loopback only; anything else must be HTTPS. Redirects are refused; there are no automatic retries or reconnects.
- Not included: stdio transport, write operations of any kind, batch mode, credential recovery.
- Resource limits: 8 MiB per response, up to 100 pages per call, 1 MiB of filtered output, 100,000 jq results.
- **Status:** verified only against local mock MCP servers (HTTP/SSE) in the test suite. It has not yet been validated against real MCP servers, and no performance claims are made.

## Development

```sh
go test ./...   # mock-server tests only; no network access to real services
go vet ./...
```

Code is formatted with gofmt/goimports. The only runtime dependency is [gojq](https://github.com/itchyny/gojq).

## License

No license has been chosen yet. Until one is added, all rights are reserved by the author.
