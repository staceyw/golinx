# GoLinx installer for Windows (PowerShell).
# Usage:  irm https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install.ps1 | iex
#   - or -
# Usage:  iex (irm https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install.ps1)

$ErrorActionPreference = "Stop"

$Repo = "staceyw/GoLinx"
$InstallDir = $PWD.Path
$BaseURL = "https://github.com/$Repo/releases/latest/download"

# --- Detect architecture ---------------------------------------------------

$Arch = if ($env:PROCESSOR_IDENTIFIER -match "ARM") {
    "arm64"
} elseif ([Environment]::Is64BitOperatingSystem) {
    "amd64"
} else {
    Write-Host "Error: 32-bit Windows is not supported." -ForegroundColor Red
    return
}

$Binary = "golinx-windows-${Arch}.exe"

# --- Pre-flight checks -----------------------------------------------------

Write-Host ""
Write-Host "GoLinx Installer" -ForegroundColor Cyan
Write-Host "================" -ForegroundColor Cyan
Write-Host ""
Write-Host "  Arch:       windows/$Arch"
Write-Host "  Binary:     $Binary"
Write-Host "  Install to: $InstallDir\"
Write-Host ""
Write-Host "This will download into ${InstallDir}\:"
Write-Host "  - golinx.exe          (binary)"
Write-Host "  - golinx.example.toml (example config)"
Write-Host "  - README.txt          (quick-start guide)"
Write-Host ""

# Prompt for confirmation
$answer = Read-Host "Continue? [Y/n]"
if ($answer -match "^[nN]") {
    Write-Host "Aborted."
    return
}

# --- Download ---------------------------------------------------------------

function Download-File($url, $dest) {
    $name = Split-Path $dest -Leaf
    Write-Host "  Downloading $name ..."
    try {
        Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing
    } catch {
        Write-Host "Error downloading ${name}: $_" -ForegroundColor Red
        throw
    }
}

Write-Host ""
Download-File "$BaseURL/$Binary" (Join-Path $InstallDir "golinx.exe")
Download-File "$BaseURL/golinx.example.toml" (Join-Path $InstallDir "golinx.example.toml")
Download-File "$BaseURL/README.txt" (Join-Path $InstallDir "README.txt")

# --- Done -------------------------------------------------------------------

Write-Host ""
Write-Host "Installed to $InstallDir\" -ForegroundColor Green
Write-Host ""
Write-Host "Quick start:"
Write-Host '  1) .\golinx.exe --listen "http://:80"'
Write-Host "  2) Click the URL in the terminal or open http://localhost in your browser."
Write-Host ""
Write-Host "For persistent config, copy golinx.example.toml to golinx.toml and run .\golinx.exe with no flags."
Write-Host ""
