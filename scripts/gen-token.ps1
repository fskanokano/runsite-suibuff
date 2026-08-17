# gen-token.ps1 - Headless token generator alias
param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Append,
    [string]$EnvFile = "",
    [string]$BaseUrl = "https://www.codebuff.com",
    [int]$TimeoutSeconds = 300,
    [int]$PollIntervalMs = 5000
)

$scriptPath = Join-Path $PSScriptRoot "gen-freebuff-token.ps1"
& $scriptPath @PSBoundParameters
