# Dashboard Guide

The embedded admin web UI gives you live relay state, per-token quota, a `.env`
config editor, logs, and metrics, in the same single binary, with no build
step and no extra process. It is htmx + Pico CSS vendored into the binary at
compile time (`internal/dashboard`); the server is untouched.

## Access

Open `http://127.0.0.1:3457/admin` (your `LISTEN_ADDR`). You land on the login
page unless `ADMIN_TOKEN` is unset.

| Setting | Behavior |
|---|---|
| `ADMIN_TOKEN` set | Login required: `ADMIN_TOKEN` is both the bearer token for `/admin/reload` and the login password. Enter it on the login page; a signed `HttpOnly` + `SameSite=Strict` cookie unlocks the dashboard for 24h (`Secure` is added automatically when the proxy listens beyond loopback). Failed logins are rate-limited per IP (5 fails → 1 minute lockout). |
| `ADMIN_TOKEN` unset | Dashboard is open (legacy behavior, matching `/admin/reload`). A startup warning reminds you to set it if the proxy is reachable beyond loopback. The **sensitive routes additionally require a loopback client** in this mode: Config and Logs (secrets), the token actions (add/remove/test/test-all), the smoke test, diagnostics, and the mode switch, so a remotely reachable proxy cannot leak or rewrite its `.env`, mutate the pool, or switch modes without the token. |

The session cookie is stateless (HMAC-signed expiry, per-process random key):
restarting the proxy signs everyone out, which is the safe default.

## Pages

- **Overview**: pooled/bridge mode, model count, uptime, safe-mode flag, and
  per-token cards: session status, ban/429 risk level (`low`/`moderate`/`high`/
  `critical`), active runs, requests, 24h messages vs `MAX_MESSAGES_PER_DAY`,
  queue position, cooldown end, transient retries. Refreshes every 5s. At the
  top, a **Smoke test** box sends one real chat through the pool (`POST
  /admin/smoke`) and reports status, the token used, latency, and a content
  preview, proving the whole path end to end.
- **Tokens**: the same per-token state in detail (session instance id) plus
  the live **per-model session quota** table (`limit`, `recent`, `period`,
  `reset`, `entitlement`) parsed from the last upstream admission, with
  **usage bars and reset countdowns** ("resets in 4h 12m", amber at ≥80%).
  Refreshes every 30s so countdowns stay honest. Per-token actions:
  - **Test**: a zero-cost upstream GET probe through that token, surfacing
    validity/network errors and live quota, the same idea as 9router's
    per-connection Test button.
  - **Unlock**: clears a cooldown / rate-limit lock / ban window (only shown
    while a lock is active; `hx-confirm` guards it. Upstream locks are
    usually correct, and unlocking a banned token resumes traffic).
  - **Finish runs**: finishes the token's active runs without touching
    in-flight requests.
  The pool is **runtime-mutable**: no restart for key changes. An **Add
  token** form (`cb_...`) appends to the live pool, **Remove last token**
  drops the highest-index token, **Test all tokens** probes every pooled
  token with a zero-cost GET probe, and **Switch to bridge mode** empties the
  pool. These map to `POST /admin/tokens/add`, `/admin/tokens/remove`,
  `/admin/tokens/test-all`, and `/admin/mode`. Every mutation is persisted
  to `AUTH_TOKENS` in `.env` and the config is reloaded, so changes survive
  a restart. Only the *last* token can be removed from the UI (removing a
  middle token would shift indices under in-flight leases). Reorder
  `AUTH_TOKENS` in Config for anything else. These actions are gated like
  Config (loopback-only when `ADMIN_TOKEN` is unset).
- **Models**: the live registry catalog, listing every model id with the upstream
  agent that serves it, plus the `MODEL_ALIASES` table.
- **Traces**: recent chat requests and their routing outcome, with token chosen,
  model, status (`ok`/`error` + class `rate_limited`/`banned`/`waiting_room`/
  `upstream`), duration, and error string. This is the ban-avoidance
  observability view: see 429/ban events as they happen. Refreshes every 3s.
- **Config**: a raw `.env` editor next to the effective-value table (secrets
  redacted to set/unset + counts). Save validates with the same pipeline as
  startup (durations, URLs, `Config.Validate`) and hot-reloads via the atomic
  config swap; invalid input is rejected **with the previous file restored**.
  When no `.env` exists yet, the editor is seeded with a commented template of
  every key and its default.
- **Setup**: a three-step wizard from zero to a working client, covering these
  steps:
  1. **Tokens**: add a token (in bridge mode the step is "add your token";
     it switches the proxy to pooled mode), remove the last, or test all.
  2. **Verify**: a **smoke test** (one real chat, `POST /admin/smoke`; in
     bridge mode it needs a client token in the payload) plus a **Full
     diagnostics** button (`POST /admin/diag`) that renders the same checks
     as `-doctor`: config state, DNS + TCP reachability, registry count, and
     a zero-cost validity probe per token.
  3. **Connect your client**: copy-paste snippets generated from the
     effective config (base URL, mode, key hint, first catalog model),
     plus the full model list as chips.
- **Logs**: the last 200 records from an in-memory ring that mirrors the
  process logger (stderr and any `LOG_FILE` still receive everything). No log
  file, no docker access needed. Refreshes every 3s.
- **Metrics**: sampled counter trends (requests, transient retries,
  fingerprint rotations) as server-rendered SVG sparklines. The full Prometheus
  exposition with per-token gauges stays at `/metrics`.

## Docker caveat

The config editor writes `./.env` **relative to the proxy's working
directory**. Inside the container that is the image workdir, not your host
directory, so in Docker, prefer environment variables in `docker-compose.yml`
(or bind-mount your `.env`) and use the dashboard's Config page read-only.
The runtime token actions (Add token, Remove last, mode switch) persist to the
same `./.env` path, so they only survive restarts when `.env` is bind-mounted
(the live pool change still applies immediately). The read-only pages
(Overview/Tokens/Logs/Metrics), the smoke test, and diagnostics work fine in
Docker.

## Hardening

1. **Set `ADMIN_TOKEN`**: a random string. Unauthenticated mode is only safe
   when the proxy is bound to loopback and unreachable from the network.
2. Keep `LISTEN_ADDR` on loopback unless you are deliberately exposing the
   proxy; if you expose it, put TLS in front (reverse proxy). The cookie and
   admin traffic would otherwise cross the wire in the clear.
3. The dashboard never renders token values: `AUTH_TOKENS`, `API_KEYS`,
   `SOCKS5_PROXY(S)`, and `ADMIN_TOKEN` show only set/unset + counts. The
   `.env` file is written atomically with mode `0600`.
4. Saves (and `/admin/reload`) re-apply the JSON config file the proxy was
   started with (`-config`), so JSON overrides survive a dashboard save.

## What is deliberately not in v1

- Multi-user/role separation: one `ADMIN_TOKEN`, one session model.
- Removing a *middle* pooled token from the UI: only the last token can be
  removed (see the Tokens page note); reorder `AUTH_TOKENS` in Config for
  anything else.

Related: [README](../README.md), [Getting Started](getting-started.md),
[Client Integration](client-integration.md), [9router Integration](9router-integration.md).
