@echo off
REM ============================================================
REM  install.bat - builds userkarmacheck.exe from the Go source
REM  Run this ONCE (or after changing any .go file).
REM  Requires Go: https://go.dev/dl/
REM ============================================================
cd /d "%~dp0"

where go >nul 2>&1
if errorlevel 1 (
  echo.
  echo ERROR: Go is not installed or not in PATH.
  echo        Install it from https://go.dev/dl/ and run this again.
  echo.
  pause
  exit /b 1
)

echo Building userkarmacheck.exe ...
go build -o userkarmacheck.exe .
if errorlevel 1 (
  echo.
  echo BUILD FAILED.
  echo.
  pause
  exit /b 1
)

echo.
echo ============================================================
echo  DONE - userkarmacheck.exe is ready.
echo.
echo    run.bat            run the fast checker (sessions + VPN)
echo    run.bat --help     all options
echo    run.bat login      open a browser to log in / save a session
echo    run.bat sessions   validate the session folder
echo ============================================================
echo.
pause
