@echo off
REM ============================================================================
REM  Start the SyncDrive daemon (background, minimized window).
REM  Log: %LOCALAPPDATA%\SyncDrive\daemon.log
REM ============================================================================
setlocal
set "ROOT=%~dp0"
set "API=http://127.0.0.1:8737"
set "LOGDIR=%LOCALAPPDATA%\SyncDrive"

REM already running?
curl -s -m 3 "%API%/api/status" >nul 2>&1
if not errorlevel 1 (
    echo SyncDrive daemon is already running on %API%.
    goto :eof
)

if not exist "%ROOT%bin\syncdrived.exe" (
    echo [ERROR] %ROOT%bin\syncdrived.exe not found. Build it first:
    echo         go build -o bin\syncdrived.exe .\core\cmd\syncdrived
    exit /b 1
)
if not exist "%ROOT%credentials.json" (
    echo [ERROR] %ROOT%credentials.json not found ^(Google OAuth client secrets^).
    exit /b 1
)
if not exist "%LOGDIR%" mkdir "%LOGDIR%"

REM Launch minimized; keep a visible-capable window (no hidden shells).
REM For a production OAuth client add:  -token-lifetime-days 0
start "SyncDrive Daemon" /min cmd /c ""%ROOT%bin\syncdrived.exe" -secrets "%ROOT%credentials.json" -port 8737 2>"%LOGDIR%\daemon.log""

REM verify it came up
timeout /t 3 /nobreak >nul
curl -s -m 5 "%API%/api/status" >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Daemon did not respond. Check "%LOGDIR%\daemon.log".
    exit /b 1
)
echo SyncDrive daemon started.  API: %API%   Log: %LOGDIR%\daemon.log
endlocal
