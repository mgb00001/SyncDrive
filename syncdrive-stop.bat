@echo off
REM ============================================================================
REM  Stop the SyncDrive daemon.
REM ============================================================================
tasklist /FI "IMAGENAME eq syncdrived.exe" | find /I "syncdrived.exe" >nul
if errorlevel 1 (
    echo SyncDrive daemon is not running.
    goto :eof
)
taskkill /IM syncdrived.exe >nul 2>&1
REM give it a moment to shut down gracefully, then force if still alive
timeout /t 3 /nobreak >nul
tasklist /FI "IMAGENAME eq syncdrived.exe" | find /I "syncdrived.exe" >nul
if not errorlevel 1 taskkill /F /IM syncdrived.exe >nul 2>&1
echo SyncDrive daemon stopped.
