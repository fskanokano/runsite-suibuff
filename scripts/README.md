# FreeBuff Auth Token Generator

`gen-freebuff-token.sh` (Linux / macOS) and `gen-freebuff-token.ps1`
(Windows) generate a FreeBuff auth token (`cb_...`) through a headless
login flow:

1. The script creates a unique `fingerprintId` and asks the FreeBuff API
   for a login URL.
2. Your browser opens for GitHub OAuth login.
3. The script polls until authentication completes (5-minute timeout).
4. The token is printed and stored according to the mode you chose.

Each run uses a fresh `fingerprintId`, so you can generate a token for a
different account by signing into that GitHub account in the browser
first.

> **Warning:** using FreeBuff tokens through a proxy violates FreeBuff /
> Codebuff terms of service. Accounts may be suspended or banned. You
> accept this risk.

## Run the proxy — one command

The extracted folder contains `freebuff-proxy` (Linux/macOS) or
`freebuff-proxy.exe` (Windows) plus this guide and the token scripts.
**One command starts everything:**

**Windows:** unzip, right-click the extracted folder → **Open in
Terminal**, then:

```powershell
.\start-proxy.cmd
```

**Linux:** unzip, right-click the extracted folder → **Open in
Terminal** (GNOME/KDE file managers), then:

```bash
./start-proxy.sh
```

`start-proxy.*` handles the whole setup automatically:

1. Creates `.env` from `.env.example` if it is missing.
2. If `AUTH_TOKENS` is empty, asks whether you want to generate a token
   now (recommended) — it opens the token generator, which appends the
   token to `.env`.
3. Starts the proxy and prints its address — point your AI client at
   `http://127.0.0.1:3457/v1`, model `deepseek/deepseek-v4-flash`.

The proxy runs in the foreground so logs are visible — press Ctrl+C to
stop. You can also run the binary directly (`./freebuff-proxy` /
`.\freebuff-proxy.exe`).

> **Windows execution policy:** if PowerShell blocks a `.ps1` with "not
> digitally signed", use the `.cmd` wrappers instead — `.\start-proxy.cmd`
> and `.\gen-token.cmd` run with `-ExecutionPolicy Bypass` and are not
> subject to the policy.

## Quick start — token generator

Run the token generator on its own (no flags) for an interactive menu —
the recommended default (Enter) appends the token to `.env` in the
current directory, auto-creating it from `.env.example` if it doesn't
exist yet:

## Requirements

- **Windows** — PowerShell 5.1+ (built into Windows 10/11)
- **Linux / macOS** — `bash`, `curl`, `jq`, `openssl`

## Windows (PowerShell)

Run the `.cmd` wrapper — it bypasses the execution policy that blocks
unsigned `.ps1` files:

```powershell
# Recommended: interactive menu — Enter appends to .\.env
.\gen-token.cmd

# Explicit modes (skip the menu):
.\gen-token.cmd -ToClipboard
.\gen-token.cmd -Save
.\gen-token.cmd -Append
.\gen-token.cmd -Append -EnvFile D:\path\to\.env
```

The `.cmd` wrapper invokes `gen-freebuff-token.ps1` with
`-ExecutionPolicy Bypass`, so no policy changes are needed. If you
prefer to call the `.ps1` directly and it is blocked, run the current
session with the policy bypassed first:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\gen-freebuff-token.ps1
```

## Linux / macOS (bash)

```bash
# Recommended: interactive menu — Enter appends to ./.env
./gen-freebuff-token.sh

# Non-interactive (piped/CI): auto-appends to ./.env
echo | ./gen-freebuff-token.sh

# Explicit modes (skip the menu):
./gen-freebuff-token.sh --clipboard
./gen-freebuff-token.sh --save
./gen-freebuff-token.sh --append
./gen-freebuff-token.sh --env /path/to/.env
```

If the script is not executable, run `chmod +x gen-freebuff-token.sh`
first (or invoke it with `bash gen-freebuff-token.sh`).

## Options

| Behavior                     | Windows                 | Linux / macOS      |
| ---------------------------- | ----------------------- | ------------------ |
| Interactive menu (default)   | *(no flags)*            | *(no flags)*       |
| Append to `.env` AUTH_TOKENS | `-Append`               | `--append`         |
| Target a specific `.env`     | `-EnvFile <path>`       | `--env <path>`     |
| Copy to clipboard            | `-ToClipboard`          | `--clipboard`      |
| Save to CLI credentials file | `-Save`                 | `--save`           |
| Print token only             | *(menu: pick 3)*        | `--print`          |

`--append` / `-Append`:
- creates `.env` from `.env.example` (searched next to the script, in
  its parent directory, then in the current directory) when it is
  missing;
- skips appending when the token is already present.

`gen-token.sh` / `gen-token.ps1` are short aliases for the same scripts,
kept in the repository for convenience.

## Next steps

- **Pooled mode** — run the script (recommended default appends
  `AUTH_TOKENS=cb_...` to `.env`), then start `freebuff-proxy`.
- **Bridge mode** — leave `AUTH_TOKENS` empty; the proxy serves tokens
  provided per request.
- Alternatively, log in once with the official CLI (`npm i -g freebuff &&
  freebuff`): the proxy auto-discovers the token from its credentials
  file on startup.
