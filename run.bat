@echo off
REM ============================================================
REM  run.bat - runs the karma checker
REM
REM    run.bat              fast run (466 sessions, VPN auto-rotate)
REM    run.bat --quiet      same, but no live panel
REM    run.bat --help       every option
REM    run.bat login        open a browser to log in / save a session
REM    run.bat sessions     validate the session folder
REM    run.bat [flags]      any check flag, e.g. -i my.txt --lanes 100
REM ============================================================
cd /d "%~dp0"
set "EXE=%~dp0userkarmacheck.exe"

if not exist "%EXE%" (
  echo userkarmacheck.exe not found - building it now ...
  go build -o "%EXE%" .
  if errorlevel 1 (
    echo BUILD FAILED - is Go installed? (or run install.bat)
    pause
    exit /b 1
  )
)

REM --- helper commands -------------------------------------
REM NOTE: a SUBCOMMAND must be passed through unchanged. The old code did
REM "%EXE%" check %* in the else-branch, so "run.bat check -i x.txt" became
REM "userkarmacheck.exe check check -i x.txt" - the second "check" was taken
REM as a positional argument, the flag parser stopped there and the run used
REM the DEFAULT input.txt instead of the one that was asked for.
if /i "%~1"=="login" (
  "%EXE%" login %2 %3 %4 %5
  pause
  exit /b 0
)
if /i "%~1"=="sessions" (
  "%EXE%" sessions %2 %3 %4 %5
  pause
  exit /b 0
)
if /i "%~1"=="fetch" (
  "%EXE%" fetch %2 %3 %4 %5
  pause
  exit /b 0
)
if /i "%~1"=="probe" (
  "%EXE%" probe %2 %3 %4 %5
  pause
  exit /b 0
)
if /i "%~1"=="fetchhtml" (
  "%EXE%" fetchhtml %2 %3 %4 %5
  pause
  exit /b 0
)
if /i "%~1"=="check" (
  "%EXE%" check %2 %3 %4 %5 %6 %7 %8 %9
  pause
  exit /b 0
)

REM --- how many session FILES do we have? --------------------------
REM The folder holds FILES (one Netscape cookie file per account), NOT
REM sub-folders - "/ad" (directories only) always counted 0.
REM NOTE: this must stay OUTSIDE the if-block below - inside a
REM parenthesised block every %VAR% is expanded when the block is PARSED,
REM so the counters would still be empty when the GTR/LSS tests run.
set "SESSION_COUNT=0"
for /f %%c in ('dir /b /a-d "%~dp0sassion\*.txt" 2^>nul ^| find /c /v ""') do set "SESSION_COUNT=%%c"
if "%SESSION_COUNT%"=="0" set "SESSION_COUNT=0"
set "LANES=%SESSION_COUNT%"
if %LANES% GTR 500 set "LANES=500"
if %LANES% LSS 10 set "LANES=10"
set "THREADS=%LANES%"
if %THREADS% GTR 150 set "THREADS=150"

REM --- default: the fast session run ------------------------
if "%~1"=="" (
  if not exist "%~dp0input.txt" (
    echo ERROR: input.txt not found in this folder.
    pause
    exit /b 1
  )
  if not exist "%~dp0sassion" (
    echo ERROR: the "sassion" folder with the cookie sessions is missing.
    echo        run "run.bat sessions" to check the folder, or "run.bat login".
    pause
    exit /b 1
  )
  echo Sessions found: %SESSION_COUNT%  |  Lanes: %LANES%  |  Threads: %THREADS%
  "%EXE%" check -i "%~dp0input.txt" -o "%~dp0full_out" ^
    --sessions "%~dp0sassion" --lanes %LANES% --threads %THREADS% ^
    --rate-per-ip 1200 --rate-per-session 72 --warmup 40 ^
    --browser-pages 4 --browser-budget 60 ^
    --adaptive-rate --source-tracking ^
    --vpn-rotate --vpn-network-lock
) else (
  "%EXE%" check %*
)
pause
