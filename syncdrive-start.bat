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

REM Roll the previous log aside (keep one prior run) so a restart never
REM destroys the evidence from the session before it.
if exist "%LOGDIR%\daemon.log" move /y "%LOGDIR%\daemon.log" "%LOGDIR%\daemon.prev.log" >nul

REM Launch minimized; keep a visible-capable window (no hidden shells).
REM -token-lifetime-days 0: production OAuth client, no 7-day token expiry
REM (set back to 7 if you ever return to a Testing-mode client).
start "SyncDrive Daemon" /min cmd /c ""%ROOT%bin\syncdrived.exe" -secrets "%ROOT%credentials.json" -port 8737 -token-lifetime-days 0 2>"%LOGDIR%\daemon.log""

REM verify it came up (ping-based delay: immune to GNU timeout shadowing
REM System32\timeout.exe when launched from Git Bash)
ping -n 4 127.0.0.1 >nul
curl -s -m 5 "%API%/api/status" >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Daemon did not respond. Check "%LOGDIR%\daemon.log".
    exit /b 1
)
echo SyncDrive daemon started.
echo   App+API: %API%  (also http://localhost:8737)
echo   Log:     %LOGDIR%\daemon.log
endlocal
