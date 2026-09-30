@echo off
setlocal
where pwsh.exe >nul 2>nul
if errorlevel 1 (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0cpa_state_relay\start.ps1" %*
) else (
  pwsh.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0cpa_state_relay\start.ps1" %*
)
if errorlevel 1 (
  echo.
  echo Startup failed. See the message above and README.md / README.zh-CN.md.
  pause
  exit /b 1
)
