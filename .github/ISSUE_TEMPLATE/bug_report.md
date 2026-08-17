---
name: Bug report
about: Something is broken: create a report to help fix it
title: ""
labels: bug
assignees: ""
---

**Describe the bug**
A clear and concise description of what is broken.

**To reproduce**
Steps to reproduce the behavior:
1. Config used (sanitized, no real tokens, replace with `cb_xxx` placeholders; do NOT paste `.env` contents)
2. Request sent (endpoint, headers, body)
3. What happened

**Expected behavior**
What you expected to happen instead.

**Environment**
- OS: [e.g. Windows 11 / Ubuntu 24.04]
- Proxy: commit hash or version, binary or Docker
- Go version (if built from source): `go version`
- Client: [9router / opencode / curl / other]

**Logs**
Relevant log lines with `LOG_LEVEL=debug` (redact tokens). If the issue is
about upstream errors (status codes like `403 free_mode_cli_required`,
`429`, `503 waiting_room_queued`), include the exact error body.

**Additional context**
Anything else that might matter.