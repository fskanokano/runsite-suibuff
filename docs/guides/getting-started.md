# Getting Started with freebuff-proxy (5-Minute Guide)

This guide takes you from zero to a working OpenAI-compatible proxy connected to FreeBuff.

---

## What is freebuff-proxy?

`freebuff-proxy` is a local bridge server. It sits between your favorite coding tools (OpenCode, pi, 9router, LiteLLM, or your own scripts) and FreeBuff's free AI models. Your tools talk standard OpenAI API to `localhost:3457`, and the proxy manages sessions and tokens behind the scenes.

```
+-------------------+      OpenAI API      +-------------------+      FreeBuff      +-------------------+
| Your AI Client    | -------------------> | freebuff-proxy    | -----------------> | codebuff.com      |
| (OpenCode / pi)  | <------------------- | (localhost:3457)  | <----------------- | (Free Models)     |
+-------------------+      SSE Streams     +-------------------+     CLI Envelope   +-------------------+
```

---

## What you'll do (the flow)

Using freebuff-proxy is five steps, most of them one command:

1. **Get a FreeBuff account + token (`cb_...`)**: the official CLI or `scripts/gen-token.*` does this for you.
2. **Install the proxy**: one command (below).
3. **Choose your mode**: one user with your own account(s) → **pooled** (`AUTH_TOKENS=cb_...`); a router serving many users → **bridge** (leave `AUTH_TOKENS=` empty).
4. **Run and verify**: `./freebuff-proxy`, then `curl http://127.0.0.1:3457/healthz`.
5. **Connect your AI tool**: point it at `http://127.0.0.1:3457/v1`, model `deepseek/deepseek-v4-flash`.

---

## Important Safety Warning

Using this proxy conflicts with Codebuff's terms of service. Upstream abuse detection scans for automation patterns and suspends accounts. Detection is documented in the open-source FreeBuff client: per-request IP scoring, per-account trust levels with sticky caps, daily spend ceilings, and mass sweeps against known farm shapes. The rules below are the evidence-backed dos and don'ts:

| ✅ Do | ❌ Don't |
|---|---|
| **Keep `SAFE_MODE=true`** (default; anti-ban stealth: TLS fingerprint, header sanitization, request jitter, idle rotation) | **Don't** run 24/7 on heavy unattended automated tasks |
| Use a **normal residential connection** | **Don't use a VPN / proxy / Tor**. Hard-block signal: limited tier or terminal `country_blocked`, restricted cohorts get a $0.50/day spend ceiling (≈1 session/day) |
| Request **only models your tier/region offers** | **Don't request out-of-region models**: refused or silently downgraded to `deepseek/deepseek-v4-flash`, and the model id is correlated with your egress IP's region |
| Keep **one modest account** | **Don't create spam clusters**: upstream caps distinct active sessions per egress IP (`ip_capped`); accounts from the same signup network (≥8 per /24) or mailbox (≥3) are permanently capped at lower trust levels |
| **Use one key until it is rate-limited** | **Don't rotate several healthy keys aggressively** (farming signal) |
| Register with a **real email address** (e.g. Gmail) | **Don't use temp-mail**. Documented ban cohort: 6,699 of 7,129 accounts on flagged domains already banned |
| Read a `429` as **quota, resets Pacific midnight** (proxy locks the token locally, answers in `<1ms`) | **Don't confuse it with a ban**: only `403` `banned`/`country_blocked` means the account is gone; use a new established account |
| Budget **4-5 keys for 24h of coding** | **Don't** expect more than one key ≈ one day of moderate use |

---

## Step 1: Install & Start the Proxy

### Option A: Portable Release ZIP (Recommended for Windows / Beginners)
1. Download the latest ZIP from [**GitHub Releases**](https://github.com/trefeon/freebuff-proxy/releases) (e.g. `freebuff-proxy_..._windows_amd64.zip`).
2. Extract the folder and:
   - **Windows**: Double-click `start-proxy.cmd`.
   - **Linux / macOS**: Open terminal in the folder and run `./start-proxy.sh`.
3. Press Enter to sign in via your browser. Your token is saved automatically!

---

### Option B: One-Command Online Installer

**Linux / macOS Terminal:**
```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

**Windows PowerShell:**
```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

---

### Option C: Docker Compose

```bash
cp .env.example .env
# Edit .env and set AUTH_TOKENS=your_token
docker compose up -d --build
```

---

## Step 2: Verify It Works

Run the diagnostic tool or curl:

```bash
# Diagnostic doctor check: config, port, DNS/TLS, registry, plus a
# zero-cost validity probe per token (no upstream session is claimed):
./freebuff-proxy -doctor

# Standalone token probe: zero-cost GET probe on the first token (no
# session claimed), prints live quota, exit 0/1 (handy for installers):
./freebuff-proxy -test-token

# Quick health check (JSON: status, uptime, model count, per-token snapshot):
curl http://localhost:3457/healthz

# Prometheus metrics scrape endpoint:
curl http://localhost:3457/metrics

# List available models:
curl http://localhost:3457/v1/models
```

`/healthz` returning status `200` means the proxy is running and reachable. It does **not** validate your token. Use `./freebuff-proxy -test-token` (or the dashboard smoke test on the Overview page) to prove a token is valid before your first chat; `-doctor` runs the same zero-cost per-token validity probe by default.

`/healthz` also reports each token's live per-model quota (`quota` map) when the last session admission carried it.

## Step 3: Connect Your Favorite AI Client

Point your AI tool to:
- **Base URL:** `http://localhost:3457/v1`
- **API Key:** `not-needed` (or your token in bridge mode)
- **Model:** `deepseek/deepseek-v4-flash`

Fastest path: run `./freebuff-proxy -setup` to write the client config automatically.

See the [Client Integration Guide](client-integration.md) for copy-paste config for OpenCode, pi, 9router, LiteLLM, and more.

---

## Troubleshooting

Run `./freebuff-proxy -doctor` to diagnose problems automatically.

| Error / Symptom | Cause & Fix |
|---|---|
| `502` + `403 free_mode_cli_required` | The request was missing the CLI system prompt marker or envelope. The proxy injects this automatically. Update to the latest version. |
| `502` + `401 Invalid API key` | Token in `.env` is expired or invalid. Catch it before the first chat: `./freebuff-proxy -test-token` (or `-doctor`) probes the token with a zero-cost GET request and fails with a clear message. Then re-run `freebuff` to log in and update `AUTH_TOKENS`, or swap the token live on the dashboard Tokens page (no restart). |
| Connection refused | Proxy is not running, or in Docker without `LISTEN_ADDR=:3457`. |
| `403 account_banned` | Account suspended upstream. Token is dead; use a new established account. |

---

## Related docs

- [Client Integration](client-integration.md): OpenCode, pi, 9router, LiteLLM, or your own scripts
- [9router Integration](9router-integration.md): wiring the proxy into 9router
- [README](../../README.md): overview, config reference, quick start
