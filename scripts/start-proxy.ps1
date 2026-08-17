# start-proxy.ps1 - Launch freebuff-proxy from the extracted folder or repo.
# Right-click this folder -> "Open in Terminal" -> .\start-proxy.cmd
# (or double-click start-proxy.cmd - it bypasses the execution policy)
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe = Join-Path $root "freebuff-proxy.exe"
if (-not (Test-Path $exe)) {
    $parentExe = Join-Path (Split-Path -Parent $root) "freebuff-proxy.exe"
    if (Test-Path $parentExe) {
        $root = Split-Path -Parent $root
        $exe = $parentExe
    }
}
Set-Location $root

if (-not (Test-Path $exe)) {
    Write-Host "freebuff-proxy.exe not found next to this script." -ForegroundColor Red
    exit 1
}

# 1. Ensure .env exists (copy from .env.example)
$envFile = Join-Path $root ".env"
if (-not (Test-Path $envFile)) {
    if (Test-Path (Join-Path $root ".env.example")) {
        Copy-Item (Join-Path $root ".env.example") $envFile
        Write-Host "No .env found; created it from .env.example" -ForegroundColor Yellow
    }
}

# 2. If no token, offer to generate one (skipped when piped/CI)
if (Test-Path $envFile) {
    $envText = [System.IO.File]::ReadAllText($envFile, [System.Text.Encoding]::UTF8)
    if ($envText -notmatch '(?m)^AUTH_TOKENS=\S') {
        $genScript = Join-Path $root "gen-freebuff-token.ps1"
        if (-not (Test-Path $genScript)) {
            $genScript = Join-Path (Join-Path $root "scripts") "gen-freebuff-token.ps1"
        }
        if (Test-Path $genScript) {
            if ([Console]::IsInputRedirected) {
                Write-Host "  No token in AUTH_TOKENS - running in bridge mode (clients send their own token)." -ForegroundColor Yellow
            } else {
                Write-Host "No token found in .env" -ForegroundColor Yellow
                $ans = Read-Host "Generate one now via browser login? [Y/n]"
                if ($ans -notmatch '^(n|no)$') {
                    & $genScript -Append -EnvFile $envFile
                } else {
                    Write-Host "  Skipped; running in bridge mode (clients send their own token)." -ForegroundColor Yellow
                }
            }
        }
    }
}

# 3. Banner with the real listen address
$addr = "127.0.0.1:3457"
if (Test-Path $envFile) {
    $line = [System.IO.File]::ReadAllText($envFile, [System.Text.Encoding]::UTF8) -split "`r?`n" |
        Where-Object { $_ -match '^LISTEN_ADDR=' } | Select-Object -First 1
    if ($line) { $addr = ($line -split '=', 2)[1].Trim() }
}
$base = "http://$addr"
Write-Host ""
Write-Host "Starting freebuff-proxy from $root" -ForegroundColor Cyan
Write-Host "  OpenAI API:  $base/v1" -ForegroundColor Green
Write-Host "  Health:      $base/healthz" -ForegroundColor Green
Write-Host "  Stop:        Ctrl+C" -ForegroundColor Green
Write-Host ""

& $exe
$code = $LASTEXITCODE
if ($code -ne 0) {
    Write-Host "freebuff-proxy exited with code $code" -ForegroundColor Red
    exit $code
}
