# Security Policy

## Supported versions

This project is a single-binary tool developed as a community proxy for an
unofficial API. Releases are cut on `v*` tags via GoReleaser with SLSA build
provenance. Only the **latest tagged release** and the latest commit on
`main` are supported. There are no long-term support (LTS) guarantees.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

- Preferred: use GitHub's **Private vulnerability reporting**
  (Security tab → "Report a vulnerability"). Public repos have this enabled
  by default on github.com. It creates a private draft advisory.
- Alternative: email the maintainer if an email address is listed on your
  dashboard profile.

What to include:
- Affected surface (e.g. HTTP endpoint, config parsing, upstream request
  handling)
- Steps to reproduce (keep any FreeBuff auth tokens out of the report: use
  placeholders)
- Impact and, if known, a suggested fix

## Scope

This proxy intentionally holds **FreeBuff auth tokens** (`AUTH_TOKENS`) and
may proxy arbitrary client requests to an upstream service. Treat anything
that could leak tokens, bypass client auth (`API_KEYS`), or be abused as an
open proxy as in scope.

In scope, in particular:

- **HTTP surface**: `/v1/chat/completions`, `/v1/models`, `POST /admin/reload`
  (reloads config from disk and can echo config errors), and the
  unauthenticated `/healthz` + `/metrics` endpoints (operational snapshot:
  uptime, model count, per-token counters; no secrets).
- **Client auth**: `API_KEYS` handling and its interplay with bridge mode
  (where `API_KEYS` is deliberately ignored because the `Authorization`
  header is the upstream token).
- **Bridge mode**: relay of client-supplied bearer tokens; anything that
  would let an unauthenticated client burn or probe another client's cached
  token slot.
- **Credential handling**: `AUTO_DISCOVER_TOKEN` reading the official CLI
  login files (`~/.config/manicode/credentials.json`,
  `~/.config/codebuff/credentials.json`), `.env`/JSON config loading, and
  `DEBUG_DUMP` dumps (redacted and mode 0600, but treat them as sensitive).
- **Supply chain**: the `-update` self-updater downloads and executes a
  release binary. It verifies the asset SHA-256 against `checksums.txt`, but
  the binary itself is a code-execution surface.

Out of scope: the FreeBuff/Codebuff upstream service itself, and behavioral
"abuse" of the free tier (quota bypassing). Report those upstream.
