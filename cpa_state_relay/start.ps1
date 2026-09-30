param(
    [ValidateRange(1024,65535)][int]$Port = 17992,
    [string]$Settings = '',
    [string]$AccountHome = '',
    [switch]$Headless,
    [switch]$NoBuild
)
$ErrorActionPreference = 'Stop'
try {
    $exe = Join-Path $PSScriptRoot 'cpa-state-relay.exe'
    $owners = @(Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique)
    if ($owners.Count -gt 0) {
        $owner = Get-Process -Id $owners[0] -ErrorAction SilentlyContinue
        if ($owners.Count -eq 1 -and $owner.Path -and [IO.Path]::GetFullPath($owner.Path) -eq [IO.Path]::GetFullPath($exe)) {
            Write-Host "CPA State is already running (PID $($owner.Id)). Use its tray icon."
            exit 0
        }
        throw "Port $Port is occupied (PID $($owners -join ', ')). No process was stopped. Close the other service or use -Port with a matching Codex base_url."
    }
    if (!(Test-Path -LiteralPath $exe)) {
        if ($NoBuild -or !(Get-Command go -ErrorAction SilentlyContinue)) {
            throw 'Executable missing. Download the Windows ZIP from GitHub Releases, or install Go 1.26+ to build this source checkout.'
        }
        Write-Host 'Building CPA State. First build may download Go dependencies...'
        Push-Location $PSScriptRoot
        try {
            & go build -trimpath -ldflags '-H windowsgui' -o $exe .
            if ($LASTEXITCODE -ne 0) { throw 'Go build failed. Check the compiler output above.' }
        } finally { Pop-Location }
    }
    $arguments = @('-listen', "127.0.0.1:$Port")
    if ($Settings) { $arguments += @('-settings', ('"' + [IO.Path]::GetFullPath($Settings) + '"')) }
    if ($AccountHome) { $arguments += @('-account-home', ('"' + [IO.Path]::GetFullPath($AccountHome) + '"')) }
    if ($Headless) { $arguments += '-headless' }
    $info = New-Object Diagnostics.ProcessStartInfo
    $info.FileName = $exe
    $info.Arguments = $arguments -join ' '
    $info.WorkingDirectory = $PSScriptRoot
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $process = [Diagnostics.Process]::Start($info)
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    do {
        if ($process.HasExited) { throw 'CPA State exited before becoming ready. Check the port, file permissions, and settings path.' }
        try {
            $status = Invoke-RestMethod "http://127.0.0.1:$Port/api/status" -TimeoutSec 1
            if ($status.PID -eq $process.Id -and $status.UIReady) {
                Write-Host "CPA State started: http://127.0.0.1:$Port (PID $($process.Id))."
                Write-Host 'Configure Codex using README.md / README.zh-CN.md. Your login/config files were not modified.'
                exit 0
            }
        } catch { }
        Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Startup check timed out for PID $($process.Id). The process was not killed. Check its tray icon and port $Port."
} catch {
    Write-Host $_.Exception.Message -ForegroundColor Red
    exit 1
}
