$ErrorActionPreference = 'Stop'
try {
    Invoke-WebRequest -Uri 'http://127.0.0.1:17992/api/quit' -Method Post -TimeoutSec 5 | Out-Null
    Write-Output 'CPA State 292 is shutting down.'
} catch {
    Write-Output 'No running relay responded; no other process was stopped.'
}
