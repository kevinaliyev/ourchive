param(
    [switch]$Agent,
    [string]$AgentConfig = "agent.json"
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

$procs = @()

try {
    Write-Host "Starting Ourchive Hub (http://localhost:8080)..." -ForegroundColor Cyan
    $procs += Start-Process -FilePath "go" -ArgumentList "run", "./cmd/hub" -NoNewWindow -PassThru

    Write-Host "Starting Ourchive Web UI (http://localhost:5173)..." -ForegroundColor Cyan
    $procs += Start-Process -FilePath "npm.cmd" -ArgumentList "--prefix", "web", "run", "dev" -NoNewWindow -PassThru

    if ($Agent) {
        if (Test-Path $AgentConfig) {
            Write-Host "Starting Ourchive Agent ($AgentConfig)..." -ForegroundColor Cyan
            $procs += Start-Process -FilePath "go" -ArgumentList "run", "./cmd/agent", "run", "--config", $AgentConfig -NoNewWindow -PassThru
        } else {
            Write-Warning "Agent config '$AgentConfig' not found. Enroll first from Contribution settings, then re-run with -Agent."
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
