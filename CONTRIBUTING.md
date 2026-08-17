# Contributing

Thanks for wanting to help! Both **issues** and **pull requests** are welcome.

This project is a community proxy for an undocumented upstream API. That
upstream changes often, so behavior can break or shift between releases.
That is expected, not a sign of neglect.

## Before you start

- Read the [README](README.md): it explains what the proxy does, how it is
  configured, and the terms-of-service risk.
- Public documentation lives in `README.md` and `docs/guides/`. Anything
  under the rest of `docs/` is local-only and gitignored; keep new docs in
  the public places.
- Planning something large? Open an issue and discuss it first. Because the
  upstream protocol is undocumented, some ideas are not feasible, and others
  (e.g. quota-related behavior) are better as config options than as
  defaults.

## Opening an issue

### Bug reports

Use the [bug report template](.github/ISSUE_TEMPLATE/bug_report.md). The
more precise the reproduction, the faster it gets fixed:

- **Sanitize everything**: no real tokens. Replace them with `cb_xxx`
  placeholders and do **not** paste `.env` contents.
- Include the config keys you changed, the exact request (endpoint, headers,
  body), and what happened vs. what you expected.
- Add your environment: OS, proxy version (or commit) and how you run it
  (binary / Docker), and the client (9router, opencode, curl, ...).
- For upstream errors (e.g. `403 free_mode_cli_required`, `429`, `503
  waiting_room_queued`), include the exact error body, ideally with
  `LOG_LEVEL=debug` lines (tokens redacted).

### Feature requests

Use the [feature request template](.github/ISSUE_TEMPLATE/feature_request.md):
what problem it solves, how the proxy should behave (config keys/endpoints in
mind), and what you tried instead. If the feature needs coordination with the
undocumented upstream API, say so, as that affects feasibility.

### Security issues

**Do not open a public issue.** Follow the [Security Policy](.github/SECURITY.md)
and use GitHub's private vulnerability reporting.

## Opening a pull request

- **Small and focused.** One logical change per PR; link the issue it fixes.
- **Fill in the PR template.** Make sure the checklist is complete:
  - `go build ./...` compiles
  - `go vet ./...` is clean
  - `go test ./...` passes (CI runs the same with `-race`)
  - Tests are added/updated when behavior changes
  - Public docs (`README.md`, `docs/guides/`) are updated when config or
    behavior changes
- **No secrets.** Never commit real FreeBuff tokens, `.env` files, or
  `config.json` contents. These are gitignored, so keep them that way.
- **CI must be green.** If a failure is unrelated to your change, say so in
  the PR description.
- **Commit style.** Conventional commits, matching the repo history:

  - `fix(scope): ...`: bug fix
  - `feat(scope): ...`: new functionality
  - `chore(scope): ...`: maintenance, tooling, deps, docs meta
  - `docs: ...`: documentation changes only

  One logical change per commit.

## What to expect

- Maintainers review on a best-effort basis; small, well-scoped PRs get
  merged fastest.
- Because the upstream protocol is undocumented, some changes cannot be
  accepted as-is and may be reworked into config options or rejected with an
  explanation.
