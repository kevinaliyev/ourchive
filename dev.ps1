param(
    [switch]$Agent,
    [string]$AgentConfig = "",
    # Start from an empty hub (first-run Host Admin setup). The existing ./data folder is moved
    # aside to ./data.bak-<timestamp>, not deleted.
    [switch]$Fresh,
    # Build the web UI into web/dist first so the hub itself serves it at http://localhost:8080.
    # The desktop app's "Sign in with your browser" opens that address, not the Vite dev server.
    [switch]$BuildWeb
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    if (Test-Path "C:\Program Files\Go\bin\go.exe") {
        $env:PATH = "C:\Program Files\Go\bin;$env:PATH"
    } else {
        Write-Error "Go was not found on PATH or in C:\Program Files\Go\bin."
        exit 1
    }
}

if ($Fresh -and (Test-Path "data")) {
    $backup = "data.bak-" + (Get-Date -Format "yyyyMMdd-HHmmss")
    Move-Item "data" $backup
    Write-Host "Moved existing hub data to ./$backup - starting with an empty hub." -ForegroundColor Yellow
}

if ($BuildWeb) {
    Write-Host "Building the web UI into web/dist..." -ForegroundColor Cyan
    npm.cmd --prefix web run build
    if ($LASTEXITCODE -ne 0) { Write-Error "Web UI build failed."; exit 1 }
} else {
    $dist = "web/dist/index.html"
    $newest = Get-ChildItem web/src -Recurse -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not (Test-Path $dist) -or ($newest -and $newest.LastWriteTime -gt (Get-Item $dist).LastWriteTime)) {
        Write-Host "web/dist is missing or older than web/src. The desktop app's browser sign-in opens http://localhost:8080, which only serves the UI from web/dist. Run .\dev.ps1 -BuildWeb to rebuild it." -ForegroundColor Yellow
    }
}

$procs = @()

try {
    Write-Host "Starting Ourchive Hub (http://localhost:8080)..." -ForegroundColor Cyan
    $procs += Start-Process -FilePath "go" -ArgumentList "run", "./cmd/hub" -NoNewWindow -PassThru

    Write-Host "Starting Ourchive Web UI (http://localhost:5173)..." -ForegroundColor Cyan
    $procs += Start-Process -FilePath "npm.cmd" -ArgumentList "--prefix", "web", "run", "dev" -NoNewWindow -PassThru

    if ($Agent) {
        $hubsDir = Join-Path $HOME ".config\ourchive\hubs"
        if ($AgentConfig -ne "" -and (Test-Path $AgentConfig)) {
            Write-Host "Starting Ourchive Agent ($AgentConfig)..." -ForegroundColor Cyan
            $procs += Start-Process -FilePath "go" -ArgumentList "run", "./cmd/agent", "run", "--config", $AgentConfig -NoNewWindow -PassThru
        } elseif ((Test-Path $hubsDir) -and ((Get-ChildItem $hubsDir -Filter "*.json" -ErrorAction SilentlyContinue).Count -gt 0)) {
            Write-Host "Starting Ourchive Agent (all enrolled hubs in $hubsDir)..." -ForegroundColor Cyan
            $procs += Start-Process -FilePath "go" -ArgumentList "run", "./cmd/agent", "run" -NoNewWindow -PassThru
        } elseif (Test-Path "agent.json") {
            Write-Host "Starting Ourchive Agent (legacy agent.json)..." -ForegroundColor Cyan
            $procs += Start-Process -FilePath "go" -ArgumentList "run", "./cmd/agent", "run", "--config", "agent.json" -NoNewWindow -PassThru
        } else {
            Write-Warning "No enrolled hubs found in '$hubsDir'. Enroll first from Contribution settings, then re-run with -Agent."
        }
    }

    Write-Host "Press Ctrl+C to stop all Ourchive processes." -ForegroundColor DarkGray
    Wait-Process -Id ($procs | ForEach-Object { $_.Id })
} finally {
    Write-Host "`nStopping Ourchive..." -ForegroundColor Yellow
    foreach ($p in $procs) {
        if ($p -and -not $p.HasExited) {
            & taskkill /PID $p.Id /T /F 2>$null | Out-Null
        }
    }
}
