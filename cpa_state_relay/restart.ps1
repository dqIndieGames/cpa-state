param(
    [Parameter(Mandatory=$true)][string]$Candidate,
    [Parameter(Mandatory=$true)][string]$ExpectedHash,
    [Parameter(Mandatory=$true)][string]$ResultDirectory,
    [string]$Target = (Join-Path $PSScriptRoot 'cpa-state-relay.exe'),
    [string]$Settings = (Join-Path $env:LOCALAPPDATA 'cpa-state-relay\settings.json'),
    [int]$Port = 17992,
    [string]$ExpectedVersion = 'multi-account-20260924',
    [int]$MinimumAccounts = 2,
    [int]$GraceSeconds = 20,
    [string]$ExpectedCurrentHash = '',
    [string]$AccountHome = '',
    [switch]$Headless,
    [switch]$RestartOnly
)
$ErrorActionPreference = 'Stop'
$Candidate = (Resolve-Path -LiteralPath $Candidate).Path
$Target = (Resolve-Path -LiteralPath $Target).Path
$ResultDirectory = [IO.Path]::GetFullPath($ResultDirectory)
$Settings = [IO.Path]::GetFullPath($Settings)
if ($Candidate -eq $Target -and -not $RestartOnly) { throw 'Candidate and target must be different files.' }
if ($RestartOnly -and $Candidate -ne $Target) { throw 'RestartOnly requires the installed executable as candidate.' }
if ((Get-FileHash -LiteralPath $Candidate -Algorithm SHA256).Hash -ne $ExpectedHash) { throw 'Candidate hash mismatch.' }
if (Test-Path -LiteralPath $ResultDirectory) { throw 'Result directory must be new.' }
New-Item -ItemType Directory -Path $ResultDirectory | Out-Null
$resultFile = Join-Path $ResultDirectory 'restart-result.json'
$logFile = Join-Path $ResultDirectory 'restart.log'
$backup = Join-Path $ResultDirectory 'previous.exe'
$settingsBackup = Join-Path $ResultDirectory 'previous-settings.json'
$base = "http://127.0.0.1:$Port"
$state = [ordered]@{Stage='preflight';Success=$false;RolledBack=$false;PID=$null;StartedAt=[DateTime]::UtcNow.ToString('o');Error=''}
function Record([string]$stage) {
    $state.Stage=$stage
    $state | ConvertTo-Json | Set-Content -LiteralPath $resultFile -Encoding utf8
    "$(Get-Date -Format o) $stage" | Add-Content -LiteralPath $logFile -Encoding utf8
}
function Listener {
    @(Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique)
}
function Wait-Exit([int]$ProcessId,[int]$Seconds) {
    $end=[DateTime]::UtcNow.AddSeconds($Seconds)
    do {
        if (-not (Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)) { return }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $end)
    throw 'Owned relay did not exit in time; target was not overwritten.'
}
function Launch {
    $arguments=@('-listen',"127.0.0.1:$Port",'-settings',('"'+$Settings+'"'))
    if ($AccountHome) { $arguments+=@('-account-home',('"'+$AccountHome+'"')) }
    if ($Headless) { $arguments+='-headless' }
    Start-Process -FilePath $Target -ArgumentList $arguments -WorkingDirectory (Split-Path -Parent $Target) -WindowStyle Hidden -PassThru
}
function Healthy([int]$ProcessId,[bool]$VerifyVersion) {
    $end=[DateTime]::UtcNow.AddSeconds(20)
    do {
        if (-not (Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)) {return $false}
        try {
            $response=Invoke-RestMethod "$base/api/status" -TimeoutSec 2
            $owners=@(Listener)
            if ($owners -contains $ProcessId) {
                if (-not $VerifyVersion) {return $true}
                if ($response.Version -eq $ExpectedVersion -and $response.PID -eq $ProcessId -and ($Headless -or $response.UIReady) -and @($response.Accounts | Where-Object Bound).Count -ge $MinimumAccounts) {return $true}
            }
        } catch { }
        Start-Sleep -Milliseconds 300
    } while ([DateTime]::UtcNow -lt $end)
    return $false
}
$oldProcessId=$null
$newProcess=$null
$replaced=$false
$settingsExisted=Test-Path -LiteralPath $Settings
try {
    Record 'preflight'
    $owners=@(Listener)
    if ($owners.Count -ne 1) {throw 'Expected exactly one relay listener.'}
    $oldProcessId=[int]$owners[0]
    $old=Get-Process -Id $oldProcessId
    if ([IO.Path]::GetFullPath($old.Path) -ne $Target) {throw 'Listener belongs to another executable; nothing stopped.'}
    Copy-Item -LiteralPath $Target -Destination $backup
    $oldHash=(Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash
    if ($ExpectedCurrentHash -and $oldHash -ne $ExpectedCurrentHash) { throw 'Installed relay changed since verification; nothing stopped.' }
    if ($settingsExisted) {Copy-Item -LiteralPath $Settings -Destination $settingsBackup}
    Record 'grace'
    Start-Sleep -Seconds $GraceSeconds
    $owners=@(Listener)
    if ($owners.Count -ne 1 -or [int]$owners[0] -ne $oldProcessId -or (Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash -ne $oldHash) {
        throw 'Relay changed during grace period; nothing stopped.'
    }
    # The complete stop/replace/start/check/rollback path runs independently of the client.
    Record 'stopping'
    # Register only observed scopes. The new process verifies exact completed
    # calls against local transcripts; no prompts or tool inputs are persisted.
    $live=Invoke-RestMethod "$base/api/status" -TimeoutSec 5
    $scopes=@{}
    foreach($session in $live.Sessions) {
        if($session.AccountID -and $session.ID) {
            $scope=[string]$session.AccountID+[char]0+[string]$session.ID+[char]0
            $hash=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($scope))).ToLowerInvariant()
            $scopes[$hash]=[DateTime]::UtcNow.AddHours(24).ToString('o')
        }
    }
    $scopePath=Join-Path (Split-Path -Parent $Settings) 'bps-restart-scopes.json'
    [IO.File]::WriteAllText($scopePath,($scopes | ConvertTo-Json -Compress),[Text.UTF8Encoding]::new($false))
    Invoke-RestMethod "$base/api/quit" -Method Post -TimeoutSec 5 | Out-Null
    Wait-Exit $oldProcessId 20
    if (@(Listener).Count -ne 0) {throw 'Port acquired by another process; replacement cancelled.'}
    if (-not $RestartOnly) {
        Record 'replacing'
        Copy-Item -LiteralPath $Candidate -Destination $Target
        $replaced=$true
    }
    if ((Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash -ne $ExpectedHash) {throw 'Installed hash mismatch.'}
    Record 'starting'
    $newProcess=Launch
    $state.PID=$newProcess.Id
    Record 'checking'
    if (-not (Healthy $newProcess.Id $true)) {throw 'New relay failed version/account/port health checks.'}
    $state.Success=$true
    Record 'complete'
} catch {
    $state.Error=$_.Exception.Message
    Record 'recovering'
    try {
        if ($newProcess -and (Get-Process -Id $newProcess.Id -ErrorAction SilentlyContinue)) {
            $owned=Get-Process -Id $newProcess.Id
            if ($owned.Path -eq $Target) {
                try {Invoke-RestMethod "$base/api/quit" -Method Post -TimeoutSec 3 | Out-Null} catch { }
                try {Wait-Exit $newProcess.Id 5} catch {Stop-Process -Id $newProcess.Id;Wait-Exit $newProcess.Id 5}
            }
        }
        if ($oldProcessId -and -not (Get-Process -Id $oldProcessId -ErrorAction SilentlyContinue) -and (Test-Path -LiteralPath $backup)) {
            if (@(Listener).Count -ne 0) {throw 'Port occupied during recovery; other process preserved.'}
            if ($replaced) {Copy-Item -LiteralPath $backup -Destination $Target}
            if ($settingsExisted) {Copy-Item -LiteralPath $settingsBackup -Destination $Settings}
            $recovered=Launch
            $state.PID=$recovered.Id
            if (-not (Healthy $recovered.Id $false)) {throw 'Previous relay did not recover.'}
            $state.RolledBack=$true
            Record 'rolled-back'
        } else {Record 'unchanged'}
    } catch {
        $state.Error+=' Recovery: '+$_.Exception.Message
        Record 'recovery-failed'
    }
    exit 1
}
