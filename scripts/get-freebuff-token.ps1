# get-freebuff-token.ps1 - legacy alias for gen-freebuff-token.ps1
# Generates a FreeBuff auth token via zero-dependency headless OAuth login.
param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Append,
    [string]$EnvFile = "",
    [switch]$Print,
    [switch]$Force,
    [switch]$Logout
)

$scriptDir = $PSScriptRoot
$genScript = Join-Path $scriptDir "gen-freebuff-token.ps1"
if (-not (Test-Path $genScript)) {
    $genScript = Join-Path $scriptDir "gen-token.ps1"
}

# The consolidated gen-freebuff-token flow implements only the switches
# forwarded below. The legacy -Logout/-Print/-Force flags were previously
# accepted and silently dropped (worst case: -Logout ignored, so stale CLI
# credentials are kept and reused on the next install). Fail loudly instead.
if ($Logout -or $Print -or $Force) {
    [Console]::Error.WriteLine("ERROR: -Logout, -Print, and -Force are not supported by the consolidated gen-freebuff-token flow.")
    [Console]::Error.WriteLine("Supported switches: -Save, -ToClipboard, -Append, -EnvFile <path>")
    [Console]::Error.WriteLine("See the usage header in $genScript (or 'Get-Help $genScript -Detailed').")
    exit 1
}

$params = @{}
if ($Save) { $params['Save'] = $true }
if ($ToClipboard) { $params['ToClipboard'] = $true }
if ($Append) { $params['Append'] = $true }
if ($EnvFile) { $params['EnvFile'] = $EnvFile }

& $genScript @params
