# xkvm one-shot installer — Windows. Downloads the latest release .zip from
# GitHub, extracts xkvm.exe to ~\.local\bin, and adds it to the current
# user's PATH. Falls back to `go install` when no release exists yet.
#
# Usage (one line, from PowerShell):
#   irm https://raw.githubusercontent.com/xscope0/xkvm-ios-injector/main/scripts/install.ps1 | iex
#
# Or with the script on disk:
#   powershell -ExecutionPolicy Bypass -File install.ps1
$ErrorActionPreference = "Stop"

$Repo    = "xscope0/xkvm-ios-injector"
$ApiUrl  = "https://api.github.com/repos/$Repo/releases/latest"
$BinDir  = Join-Path $env:USERPROFILE ".local\bin"
$BinPath = Join-Path $BinDir "xkvm.exe"

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
Write-Host "xkvm: installing to $BinDir"

$Release = $null
try {
    $Release = Invoke-RestMethod -Uri $ApiUrl -Headers @{ "User-Agent" = "xkvm-installer" }
} catch { }

if ($null -eq $Release -or $null -eq $Release.tag_name) {
    Write-Host "xkvm: no release found yet — falling back to 'go install' (needs Go 1.26+)."
    $Go = Get-Command go -ErrorAction SilentlyContinue
    if ($null -eq $Go) {
        Write-Host "xkvm: 'go' not found. Install Go (https://go.dev/dl/) then re-run, or wait for a release." -ForegroundColor Red
        exit 1
    }
    go install "github.com/xscope0/xkvm-ios-injector/cmd/xkvm@latest"
    Write-Host "xkvm: installed via 'go install'."
    exit 0
}

$Tag   = $Release.tag_name
$Ver   = $Tag.TrimStart("v")
$Asset = "xkvm_${Ver}_windows_amd64.zip"
$Url   = "https://github.com/$Repo/releases/download/$Tag/$Asset"

Write-Host "xkvm: fetching $Tag ($Asset)"
$Tmp = Join-Path $env:TEMP "xkvm-install-$(Get-Random)"
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null

try {
    Invoke-WebRequest -Uri $Url -OutFile (Join-Path $Tmp "xkvm.zip") -UseBasicParsing
    Expand-Archive -Path (Join-Path $Tmp "xkvm.zip") -DestinationPath $Tmp -Force

    $Exe = Get-ChildItem -Path $Tmp -Filter "xkvm.exe" -Recurse | Select-Object -First 1
    if ($null -eq $Exe) {
        throw "archive did not contain xkvm.exe"
    }
    Copy-Item $Exe.FullName $BinPath -Force
} finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}

Write-Host "xkvm: installed at $BinPath"

# Add to the current user's PATH if missing.
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($UserPath -notlike "*$BinDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$UserPath;$BinDir", "User")
    $env:Path = "$env:Path;$BinDir"
    Write-Host "xkvm: added $BinDir to your PATH (new terminals only)."
}

Write-Host "xkvm: done. Run 'xkvm tui' to get started."
