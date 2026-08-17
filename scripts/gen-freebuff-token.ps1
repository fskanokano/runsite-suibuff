# gen-freebuff-token.ps1 - Generate a FreeBuff auth token via headless login flow
#
# Usage:
#   .\gen-freebuff-token.ps1                  # interactive: recommends options; Enter = auto-append to .\.env
#   .\gen-freebuff-token.ps1 -ToClipboard     # generate token and copy to clipboard
#   .\gen-freebuff-token.ps1 -Save            # save to ~/.config/manicode/credentials.json
#   .\gen-freebuff-token.ps1 -Append          # append to .env AUTH_TOKENS (auto-copies .env.example if missing)
#   .\gen-freebuff-token.ps1 -EnvFile D:\.env # target .env file for -Append
# Flow:
#   1. POST /api/auth/cli/code  → gets loginUrl + fingerprintHash
#   2. Opens browser for GitHub OAuth login
#   3. Polls /api/auth/cli/status until authenticated (5min timeout)
#   4. Extracts authToken and saves/prints it
#
# Each run generates a unique fingerprintId so multiple accounts can coexist.
# Log into a DIFFERENT GitHub account in your browser before running to get
# a token for that account.
#
# WARNING: Using FreeBuff tokens through a proxy violates FreeBuff/Codebuff
# terms of service. Accounts may be suspended or banned. You accept this risk.

param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Append,
    [string]$EnvFile = "",
    [string]$BaseUrl = $(if ($env:FREEBUFF_BASE_URL) { $env:FREEBUFF_BASE_URL } else { "https://www.codebuff.com" }),
    [int]$TimeoutSeconds = 300,
    [int]$PollIntervalMs = 5000
)

$ErrorActionPreference = "Stop"

# --- helpers -----------------------------------------------------------------
function Generate-FingerprintId {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $hash = [Convert]::ToBase64String($bytes) -replace '[+/=]', ''
    return "enhanced-$($hash.Substring(0, [Math]::Min(43, $hash.Length)))"
}

function Get-ConfigDir {
    return Join-Path $env:USERPROFILE ".config\manicode"
}

function Get-CredentialsPath {
    return Join-Path (Get-ConfigDir) "credentials.json"
}

function Ensure-EnvFile {
    param([string]$Path)
    if (Test-Path $Path) { return }
    $scriptDir = $PSScriptRoot
    $candidates = @(
        (Join-Path $scriptDir ".env.example"),
        (Join-Path (Split-Path -Parent $scriptDir) ".env.example"),
        (Join-Path (Get-Location) ".env.example")
    )
    foreach ($candidate in $candidates) {
        if (Test-Path $candidate) {
            Copy-Item $candidate $Path
            Write-Host "  No .env found; created $Path from $candidate" -ForegroundColor Yellow
            return
        }
    }
    New-Item -ItemType File -Path $Path -Force | Out-Null
    Write-Host "  No .env found; created empty $Path" -ForegroundColor Yellow
}

# --- 0. warning --------------------------------------------------------------
Write-Host ""
Write-Host "FreeBuff Token Generator" -ForegroundColor Cyan
Write-Host "WARNING: Using tokens through a proxy violates FreeBuff ToS." -ForegroundColor Yellow
Write-Host "Accounts may be suspended or banned. You accept this risk." -ForegroundColor Yellow
Write-Host ""

# --- 0.5 recommend options for easier usage ----------------------------------
if (-not $Save -and -not $ToClipboard -and -not $Append) {
    if ([Console]::IsInputRedirected) {
        # Non-interactive (piped/CI): auto-append to the current .env
        $Append = $true
    } else {
        Write-Host "Recommended options:" -ForegroundColor Cyan
        Write-Host "  [Enter]  Append token to .\.env (auto-copy .env.example if missing)" -ForegroundColor Green
        Write-Host "  1)       Copy token to clipboard" -ForegroundColor Gray
        Write-Host "  2)       Save to ~/.config/manicode/credentials.json" -ForegroundColor Gray
        Write-Host "  3)       Print token only" -ForegroundColor Gray
        $choice = Read-Host "Choose [Enter]"
        switch ($choice.Trim().ToLower()) {
            { $_ -in "", "append", "a" } { $Append = $true }
            { $_ -in "1", "clipboard", "c" } { $ToClipboard = $true }
            { $_ -in "2", "save", "s" } { $Save = $true }
            { $_ -in "3", "print", "p" } { }
            default {
                Write-Host "Unknown choice '$choice'; using recommended (append)." -ForegroundColor Yellow
                $Append = $true
            }
        }
        Write-Host ""
    }
}

# --- 1. generate fingerprint + request login URL -----------------------------
$fingerprintId = Generate-FingerprintId
Write-Host "Fingerprint: $fingerprintId" -ForegroundColor DarkGray

Write-Host "Requesting login URL..." -ForegroundColor Cyan
try {
    $codeBody = @{ fingerprintId = $fingerprintId } | ConvertTo-Json
    $codeResp = Invoke-RestMethod -Uri "$BaseUrl/api/auth/cli/code" `
        -Method POST `
        -ContentType "application/json" `
        -Body $codeBody
} catch {
    Write-Host "Failed to get login URL: $_" -ForegroundColor Red
    exit 1
}

$loginUrl = $codeResp.loginUrl
$fingerprintHash = $codeResp.fingerprintHash
$expiresAt = $codeResp.expiresAt

if (-not $loginUrl) {
    Write-Host "No loginUrl in response. Server may be down." -ForegroundColor Red
    exit 1
}

# --- 2. open browser ---------------------------------------------------------
Write-Host ""
Write-Host "Opening browser for GitHub login..." -ForegroundColor Green
Write-Host "URL: $loginUrl" -ForegroundColor DarkGray
Write-Host ""
Write-Host "  -> Log in with the GitHub account you want a token for." -ForegroundColor Yellow
Write-Host "  -> If you want a DIFFERENT account, sign out of GitHub first!" -ForegroundColor Yellow
Write-Host ""
Start-Process $loginUrl

# --- 3. poll for auth completion ---------------------------------------------
Write-Host "Waiting for login (timeout: ${TimeoutSeconds}s)..." -ForegroundColor Cyan
$startTime = Get-Date
$attempts = 0

while ($true) {
    $elapsed = ((Get-Date) - $startTime).TotalSeconds
    if ($elapsed -ge $TimeoutSeconds) {
        Write-Host "Login timed out after ${TimeoutSeconds}s." -ForegroundColor Red
        exit 1
    }

    $attempts++
    Start-Sleep -Milliseconds $PollIntervalMs

    try {
        $query = "fingerprintId=$([Uri]::EscapeDataString($fingerprintId))&fingerprintHash=$([Uri]::EscapeDataString($fingerprintHash))&expiresAt=$([Uri]::EscapeDataString($expiresAt))"
        $statusResp = Invoke-RestMethod -Uri "$BaseUrl/api/auth/cli/status?$query" `
            -Method GET `
            -ContentType "application/json" `
            -ErrorAction SilentlyContinue
    } catch {
        $statusCode = $_.Exception.Response.StatusCode.value__
        if ($statusCode -eq 401) {
            # Not yet authenticated - keep polling
            Write-Host "  Polling ($attempts)... not yet authenticated" -ForegroundColor DarkGray
            continue
        }
        Write-Host "  Polling error ($attempts): $_" -ForegroundColor DarkGray
        continue
    }

    if ($statusResp.user -and $statusResp.user.authToken) {
        $user = $statusResp.user
        break
    }
    Write-Host "  Polling ($attempts)... waiting for browser login" -ForegroundColor DarkGray
}

# --- 4. extract token --------------------------------------------------------
$authToken = $user.authToken
$userName = if ($user.name) { $user.name } else { "unknown" }
$userEmail = if ($user.email) { $user.email } else { "unknown" }

Write-Host ""
Write-Host "Login successful!" -ForegroundColor Green
Write-Host "  Account: $userName ($userEmail)" -ForegroundColor Cyan
Write-Host "  Token:   $authToken" -ForegroundColor White

# --- 5. save credentials locally (opt-in only with -Save) ---------------------
if ($Save) {
    $configDir = Get-ConfigDir
    if (-not (Test-Path $configDir)) {
        New-Item -ItemType Directory -Path $configDir -Force | Out-Null
    }
    $credPath = Get-CredentialsPath
    $credData = @{
        default = @{
            id = $user.id
            name = $userName
            email = $userEmail
            authToken = $authToken
            fingerprintId = $fingerprintId
            fingerprintHash = $fingerprintHash
        }
    } | ConvertTo-Json -Depth 5
    [System.IO.File]::WriteAllText($credPath, $credData, (New-Object System.Text.UTF8Encoding($false)))
    Write-Host "  Saved to: $credPath" -ForegroundColor DarkGray
}

# --- 6. output options -------------------------------------------------------
if ($ToClipboard) {
    Set-Clipboard -Value $authToken
    Write-Host "  Copied to clipboard!" -ForegroundColor Green
}

if ($Append) {
    $targetEnv = if ($EnvFile) { $EnvFile } else { Join-Path (Get-Location) ".env" }
    if (-not (Test-Path $targetEnv)) {
        Ensure-EnvFile -Path $targetEnv
    }
    $content = [System.IO.File]::ReadAllText($targetEnv, [System.Text.Encoding]::UTF8)
    if ($authToken -and $content -like "*$authToken*") {
        Write-Host "  Token already present in $targetEnv; skipped." -ForegroundColor DarkGray
    } elseif ($content -match '(?m)^AUTH_TOKENS=(.*)$') {
        $existing = $Matches[1].Trim()
        if ($existing -and $existing -ne "") {
            $newValue = "$existing,$authToken"
        } else {
            $newValue = $authToken
        }
        $content = $content -replace '(?m)^AUTH_TOKENS=.*$', "AUTH_TOKENS=$newValue"
        [System.IO.File]::WriteAllText($targetEnv, $content, (New-Object System.Text.UTF8Encoding($false)))
        Write-Host "  Appended to: $targetEnv" -ForegroundColor Green
    } else {
        $content += "`nAUTH_TOKENS=$authToken`n"
        [System.IO.File]::WriteAllText($targetEnv, $content, (New-Object System.Text.UTF8Encoding($false)))
        Write-Host "  Appended to: $targetEnv" -ForegroundColor Green
    }
}

Write-Host ""
Write-Host "Done! Add this token to your 9router or .env AUTH_TOKENS." -ForegroundColor Cyan
Write-Host ""
