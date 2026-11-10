@echo off
REM ============================================================
REM  run_noproxy.bat - NO PROXY mode with ExpressVPN auto-rotation
REM
REM  Every request goes through your own IP. The VPN rotates the exit
REM  IP automatically when a rate limit is detected, so the run keeps
REM  going instead of stalling at ~6-7k accounts.
REM
REM    run_noproxy.bat            normal run
REM    run_noproxy.bat --debug    + rate-bucket / source-health panels
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

echo.
echo ============================================================
echo  NO-PROXY MODE
echo    - single exit IP, conservative adaptive rate
echo    - ExpressVPN auto-rotation on rate limit (another country)
echo    - adaptive per-group rate control
echo    - every lane gets its OWN session; blocked sessions are
echo      swapped for a fresh one from the pool immediately
echo ============================================================
echo.

REM --- how many session FILES do we have? --------------------------
REM NOTE: the session folder holds FILES (one Netscape cookie file per
REM account), NOT sub-folders - "/ad" (directories only) always counted 0
REM and the run silently fell back to 10 lanes.
set "SESSION_COUNT=0"
for /f %%c in ('dir /b /a-d "%~dp0sassion\*.txt" 2^>nul ^| find /c /v ""') do set "SESSION_COUNT=%%c"
if "%SESSION_COUNT%"=="0" set "SESSION_COUNT=0"
echo Sessions found: %SESSION_COUNT%

REM --- one lane per session, capped at 80 (memory: 1 TLS client each)
set "LANES=%SESSION_COUNT%"
if %LANES% GTR 80 set "LANES=80"
if %LANES% LSS 10 set "LANES=10"
echo Lanes / threads: %LANES%
echo.

"%EXE%" check -i "%~dp0input.txt" -o "%~dp0full_out" ^
  --sessions "%~dp0sassion" ^
  --multi-engine --engine-workers %LANES% ^
  --lanes %LANES% --threads %LANES% ^
  --rate-per-ip 300 ^
  --rate-per-session 30 ^
  --warmup 30 ^
  --adaptive-rate ^
  --min-rate 15 --max-rate 50 --rate-speed 0.2 ^
  --source-tracking ^
  --browser-pages 2 --browser-budget 40 ^
  --vpn-rotate --vpn-network-lock ^
  --vpn-locations "%~dp0vpn_locations.txt" ^
  --vpn-threshold 3 --vpn-min-interval 30 %*

pause