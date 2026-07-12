@echo off
setlocal EnableDelayedExpansion
REM ============================================================================
REM  SyncDrive overnight spillover stress test
REM
REM  1. Creates a test folder OUTSIDE OneDrive (30GB of churn must not hit it)
REM  2. Registers it as a SyncDrive mirror via the daemon API
REM  3. Creates 1GB text files one at a time until 30GB total, pausing a sync
REM     cycle between files so each is picked up individually.
REM
REM  Expected overnight result: as each Google account drops below 20%% free,
REM  new files spill to the next connected account. Watch for "provisioned
REM  spillover target" lines in the daemon log and the storage bars on the
REM  Accounts page (http://localhost:1420).
REM
REM  REQUIREMENTS
REM    - syncdrived must be running:  bin\syncdrived.exe -secrets credentials.json
REM    - ~31GB free on C:
REM ============================================================================

set "TESTROOT=C:\Users\mgb00\SyncDriveStress"
set "API=http://127.0.0.1:8737"
set "ACCOUNT=project00003@gmail.com"
set "REMOTE_NAME=SyncDriveStress"
set "FILE_COUNT=30"
REM seconds to wait between files (one daemon poll cycle is 60s)
set "PAUSE_SECONDS=70"

echo ============================================================
echo  SyncDrive spillover stress test
echo  started: %date% %time%
echo  plan: %FILE_COUNT% x 1GB files into %TESTROOT%
echo ============================================================

REM ---- 1. daemon must be up --------------------------------------------------
curl -s -m 5 "%API%/api/status" >nul 2>&1
if errorlevel 1 (
    echo [ERROR] SyncDrive daemon is not reachable on %API%.
    echo         Start it first:  bin\syncdrived.exe -secrets credentials.json
    exit /b 1
)
echo [ok] daemon reachable

REM ---- 2. create the local folder --------------------------------------------
if not exist "%TESTROOT%" mkdir "%TESTROOT%"
echo [ok] local folder: %TESTROOT%

REM ---- 3. register the mirror (skip if already registered) --------------------
curl -s "%API%/api/folders" | findstr /C:"SyncDriveStress" >nul 2>&1
if errorlevel 1 (
    curl -s -X POST "%API%/api/folders" -d "{\"local_root_path\":\"C:/Users/mgb00/SyncDriveStress\",\"account\":\"%ACCOUNT%\",\"remote_folder_name\":\"%REMOTE_NAME%\",\"holding_period_days\":30}"
    echo.
    echo [ok] mirror registered on %ACCOUNT%
) else (
    echo [ok] mirror already registered - reusing it
)

REM ---- 4. build a ~64KB seed block of text ------------------------------------
set "SEED=%TESTROOT%\_seed.txt"
if exist "%SEED%" del "%SEED%"
for /L %%i in (1,1,512) do (
    >>"%SEED%" echo SyncDrive stress test payload line %%i - the quick brown fox jumps over the lazy dog 0123456789 ABCDEFGHIJKLMNOPQRSTUVWXYZ.
)
echo [ok] seed block created

REM ---- 5. build a 256MB chunk by doubling the seed (12 doublings) --------------
set "CHUNK=%TESTROOT%\_chunk.txt"
copy /y /b "%SEED%" "%CHUNK%" >nul
for /L %%i in (1,1,12) do (
    copy /y /b "%CHUNK%"+"%CHUNK%" "%TESTROOT%\_chunk.tmp" >nul
    move /y "%TESTROOT%\_chunk.tmp" "%CHUNK%" >nul
)
for %%A in ("%CHUNK%") do echo [ok] chunk ready: %%~zA bytes

REM ---- 6. create 1GB files (4 chunks each), one per sync cycle -----------------
echo [run] creating %FILE_COUNT% x 1GB files - press Ctrl+C to stop
for /L %%n in (1,1,%FILE_COUNT%) do (
    if exist "%TESTROOT%\stress-part-%%n.txt" (
        echo   %time%  stress-part-%%n.txt already exists - skipping
    ) else (
        copy /y /b "%CHUNK%"+"%CHUNK%"+"%CHUNK%"+"%CHUNK%" "%TESTROOT%\stress-part-%%n.txt" >nul
        echo   %time%  created stress-part-%%n.txt
        timeout /t %PAUSE_SECONDS% /nobreak >nul
    )
)

echo ============================================================
echo  finished: %date% %time%
echo  final per-account storage:
curl -s "%API%/api/accounts"
echo.
echo  folder targets (look for multiple accounts on SyncDriveStress):
curl -s "%API%/api/folders"
echo.
echo  check the daemon log for "provisioned spillover target" events.
echo ============================================================
endlocal
