@echo off
REM ============================================================================
REM  Launch SyncDrive as a system-tray app (no terminal window).
REM  The tray icon's menu can open the UI, show logs, and shut down.
REM  Put a shortcut to this in shell:startup to run it at login.
REM ============================================================================
setlocal
set "ROOT=%~dp0"
set "API=http://127.0.0.1:8737"
set "LOGDIR=%LOCALAPPDATA%\SyncDrive"

REM already running?
curl -s -m 3 "%API%/api/status" >nul 2>&1
if not errorlevel 1 (
    echo SyncDrive is already running. Look for the tray icon.
    goto :eof
)
if not exist "%ROOT%bin\syncdrive-tray.exe" (
    echo [ERROR] bin\syncdrive-tray.exe not found. Build it:
    echo   go build -ldflags "-H=windowsgui" -o bin\syncdrive-tray.exe .\core\cmd\syncdrived
    exit /b 1
)
if not exist "%LOGDIR%" mkdir "%LOGDIR%"
if exist "%LOGDIR%\daemon.log" move /y "%LOGDIR%\daemon.log" "%LOGDIR%\daemon.prev.log" >nul

REM Launch the windowless GUI build; this .bat returns immediately and the
REM app lives in the system tray. -token-lifetime-days 0 = production client.
start "" "%ROOT%bin\syncdrive-tray.exe" -tray -secrets "%ROOT%credentials.json" -port 8737 -token-lifetime-days 0 -log "%LOGDIR%\daemon.log"
echo SyncDrive is starting in the system tray. Click the icon for its menu.
endlocal
