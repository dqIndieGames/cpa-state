param([string]$OutputDirectory = '')
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
if (!$OutputDirectory) { $OutputDirectory = Join-Path $root 'dist' }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
$bundle = Join-Path $OutputDirectory 'cpa-state-windows-amd64'
$zip = Join-Path $OutputDirectory 'cpa-state-windows-amd64.zip'
if ((Test-Path -LiteralPath $bundle) -or (Test-Path -LiteralPath $zip)) { throw 'Output already exists. Choose a new -OutputDirectory; no files were removed.' }
if (!(Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go 1.26+ is required.' }
New-Item -ItemType Directory -Path (Join-Path $bundle 'cpa_state_relay') -Force | Out-Null
Push-Location (Join-Path $root 'cpa_state_relay')
try {
    # Environment overrides are local to this process and restored below.
    $priorOS=$env:GOOS; $priorArch=$env:GOARCH; $priorCGO=$env:CGO_ENABLED
    $env:GOOS='windows'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
    & go build -trimpath -ldflags '-H windowsgui' -o (Join-Path $bundle 'cpa_state_relay\cpa-state-relay.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'Release build failed.' }
} finally { $env:GOOS=$priorOS; $env:GOARCH=$priorArch; $env:CGO_ENABLED=$priorCGO; Pop-Location }
foreach ($name in @('start.cmd','README.md','README.zh-CN.md','LICENSE','THIRD_PARTY_NOTICES.md')) {
    Copy-Item -LiteralPath (Join-Path $root $name) -Destination $bundle
}
foreach ($name in @('start.ps1','start.cmd','restart.ps1','stop.ps1','README.md')) {
    Copy-Item -LiteralPath (Join-Path $root "cpa_state_relay\$name") -Destination (Join-Path $bundle 'cpa_state_relay')
}
New-Item -ItemType Directory -Path (Join-Path $bundle 'examples') | Out-Null
Copy-Item -LiteralPath (Join-Path $root 'examples\codex-config.toml') -Destination (Join-Path $bundle 'examples')
Copy-Item -LiteralPath (Join-Path $root 'third-party-licenses') -Destination $bundle -Recurse
Compress-Archive -Path (Join-Path $bundle '*') -DestinationPath $zip
$hash=(Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText((Join-Path $OutputDirectory 'SHA256SUMS.txt'), "$hash  cpa-state-windows-amd64.zip`n", (New-Object Text.UTF8Encoding($false)))
Write-Output $zip
