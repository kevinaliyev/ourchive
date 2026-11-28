# Windows/PowerShell twin of bootstrap-build-assets.sh: generates client/build/ (platform
# Taskfiles, NSIS installer script, icons, ourchive:// registration) with the Wails CLI, using
# build/config.ourchive.yml for product metadata. Run once after cloning from the client/ folder:
#
#   go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
#   .\scripts\bootstrap-build-assets.ps1
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

if (-not (Get-Command wails3 -ErrorAction SilentlyContinue)) {
    $bin = Join-Path (& go env GOPATH) "bin"
    if (Test-Path (Join-Path $bin "wails3.exe")) {
        $env:Path += ";$bin"
    } else {
        throw "wails3 not found. Run: go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0"
    }
}

function Invoke-Wails {
    & wails3 @args
    if ($LASTEXITCODE -ne 0) { throw "wails3 $($args -join ' ') failed (exit $LASTEXITCODE)" }
}

Invoke-Wails generate build-assets `
    -dir build `
    -name ourchive `
    -binaryname ourchive `
    -productname "Ourchive" `
    -productcompany "Ourchive" `
    -productidentifier com.ourchive.client `
    -productdescription "Ourchive desktop client" `
    -productversion 0.4.0 `
    -productcopyright "(c) Ourchive contributors" `
    -silent

Copy-Item build/config.ourchive.yml build/config.yml -Force
Invoke-Wails update build-assets -dir build -config build/config.yml -silent

if (-not (Test-Path build/Taskfile.yml)) { throw "build/Taskfile.yml was not generated" }
Write-Host "Build assets generated in client/build (ourchive:// registered)."
