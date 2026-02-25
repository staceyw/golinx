# Cross-compile binaries and create a GitHub release.
# Usage:  .\scripts\release.ps1 v0.3.0
#         .\scripts\release.ps1 v0.3.0 -DryRun   # build only, no upload

param(
    [Parameter(Position=0)]
    [string]$Tag,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

if (-not $Tag) {
    $latest = (gh release list --limit 1 --json tagName 2>$null | ConvertFrom-Json)
    if ($latest -and $latest.Count -gt 0) {
        Write-Host "Latest release: $($latest[0].tagName)"
    }
    $Tag = Read-Host "Enter version tag (e.g. v0.3.0)"
    if (-not $Tag) { throw "Version tag is required" }
}
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$dist = Join-Path $root "dist"

if (!(Test-Path $dist)) { New-Item -ItemType Directory -Path $dist | Out-Null }

$targets = @(
    @{ GOOS="windows"; GOARCH="amd64"; Out="golinx-windows-amd64.exe" },
    @{ GOOS="windows"; GOARCH="arm64"; Out="golinx-windows-arm64.exe" },
    @{ GOOS="linux";   GOARCH="amd64"; Out="golinx-linux-amd64" },
    @{ GOOS="linux";   GOARCH="arm64"; Out="golinx-linux-arm64" },
    @{ GOOS="darwin";  GOARCH="arm64"; Out="golinx-darwin-arm64" }
)

# Build binaries
Write-Host "Building $($targets.Count) targets ..."
Push-Location $root
try {
    foreach ($t in $targets) {
        $env:GOOS   = $t.GOOS
        $env:GOARCH = $t.GOARCH
        $out = Join-Path $dist $t.Out
        Write-Host "  $($t.GOOS)/$($t.GOARCH) -> $($t.Out)"
        go build -ldflags "-s -w -X main.Version=$Tag" -o $out .
        if ($LASTEXITCODE -ne 0) { throw "Build failed for $($t.GOOS)/$($t.GOARCH)" }
    }
} finally {
    Remove-Item Env:\GOOS  -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}

# Run tests
Write-Host ""
Write-Host "Running tests ..."
Push-Location $root
try {
    go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw "Tests failed" }
} finally { Pop-Location }

# Copy common files as standalone release assets (used by install scripts)
Write-Host ""
Write-Host "Preparing common assets ..."
$example = Join-Path $root "golinx.example.toml"

$readme = Join-Path $dist "README.txt"
@"
GoLinx - URL shortener and people directory

Quick Start:
  1. Copy golinx.example.toml to golinx.toml
  2. Edit golinx.toml - add at least one listener (e.g. http://:8080)
  3. Run:  ./golinx
  4. Open: http://localhost:8080

Full documentation: https://github.com/staceyw/GoLinx
"@ | Set-Content -Path $readme -Encoding UTF8

$config = Join-Path $dist "golinx.example.toml"
Copy-Item $example $config

# Collect all assets (binaries + common files)
$assets = @()
foreach ($t in $targets) {
    $assets += Join-Path $dist $t.Out
}
$assets += $readme
$assets += $config

Write-Host "  README.txt"
Write-Host "  golinx.example.toml"

if ($DryRun) {
    Write-Host ""
    Write-Host "Dry run complete. Artifacts in: $dist"
    Write-Host "  Binaries: $($targets.Count)"
    Write-Host "  Common:   README.txt, golinx.example.toml"
    Write-Host "Re-run without -DryRun to upload to GitHub."
    return
}

# Create release
Write-Host ""
Write-Host "Creating release $Tag ..."
$notes = @"
## Option 1: Install Script

Run one command to download everything into the current directory (binary, config template, and quick-start README).

**Linux / macOS:**
``````
curl -fsSL https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install.sh | bash
``````

**Windows (PowerShell):**
``````
iex (irm https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install.ps1)
``````

## Option 2: Manual Download

Pick your platform binary below, plus ``golinx.example.toml`` and ``README.txt``.

| File | Description |
|------|-------------|
| ``golinx-windows-amd64.exe`` | Windows x64 |
| ``golinx-windows-arm64.exe`` | Windows ARM64 |
| ``golinx-linux-amd64`` | Linux x64 |
| ``golinx-linux-arm64`` | Linux ARM64 / Raspberry Pi |
| ``golinx-darwin-arm64`` | macOS Apple Silicon |
| ``golinx.example.toml`` | Example configuration file |
| ``README.txt`` | Quick-start guide |
"@
gh release create $Tag @assets --title "GoLinx $Tag" --generate-notes --notes $notes
if ($LASTEXITCODE -ne 0) { throw "gh release create failed" }

Write-Host ""
Write-Host "Done: https://github.com/staceyw/GoLinx/releases/tag/$Tag"
