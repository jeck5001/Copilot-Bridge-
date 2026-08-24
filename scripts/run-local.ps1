param(
    [string]$Binary = ""
)

$ErrorActionPreference = "Stop"
$ProjectRoot = Split-Path -Parent $PSScriptRoot
$DataRoot = Join-Path $ProjectRoot "data"
New-Item -ItemType Directory -Path $DataRoot -Force | Out-Null

if ([string]::IsNullOrWhiteSpace($env:M365_LISTEN)) { $env:M365_LISTEN = "127.0.0.1:4141" }
if ([string]::IsNullOrWhiteSpace($env:M365_TOKEN_CACHE)) { $env:M365_TOKEN_CACHE = Join-Path $DataRoot "accounts.json" }
if ([string]::IsNullOrWhiteSpace($env:M365_SESSION_CACHE)) { $env:M365_SESSION_CACHE = Join-Path $DataRoot "sessions.json" }
if ([string]::IsNullOrWhiteSpace($env:M365_API_KEYS)) { $env:M365_API_KEYS = Join-Path $DataRoot "api-keys.json" }
if ([string]::IsNullOrWhiteSpace($env:M365_SETTINGS_FILE)) { $env:M365_SETTINGS_FILE = Join-Path $DataRoot "settings.json" }
if ([string]::IsNullOrWhiteSpace($env:M365_DEBUG_LOG)) { $env:M365_DEBUG_LOG = Join-Path $DataRoot "debug-logs.jsonl" }
if ([string]::IsNullOrWhiteSpace($env:M365_ADMIN_PASSWORD_HASH_FILE)) { $env:M365_ADMIN_PASSWORD_HASH_FILE = Join-Path $DataRoot "admin-password.hash" }
if ([string]::IsNullOrWhiteSpace($env:M365_ADMIN_PASSWORD)) { $env:M365_ADMIN_PASSWORD = "admin888" }
if ([string]::IsNullOrWhiteSpace($env:M365_COOKIE_SECURE)) { $env:M365_COOKIE_SECURE = "false" }
if ([string]::IsNullOrWhiteSpace($env:M365_LOG_LEVEL)) { $env:M365_LOG_LEVEL = "warn" }

Push-Location $ProjectRoot
try {
    if ([string]::IsNullOrWhiteSpace($Binary)) {
        & go run ./cmd/server
    } else {
        $ResolvedBinary = (Resolve-Path -LiteralPath $Binary).Path
        & $ResolvedBinary
    }
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
