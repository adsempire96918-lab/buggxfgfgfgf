@echo off
REM ============================================================
REM  test_vpn.bat - proves the ExpressVPN auto-rotation works
REM
REM  Rotates the REAL VPN once and prints the old and the new exit IP.
REM  Use it before a long run to be sure the rotation path works.
REM ============================================================
cd /d "%~dp0"

echo.
echo === ExpressVPN status ===
"C:\Program Files\ExpressVPN\expressvpnctl.exe" get connectionstate
"C:\Program Files\ExpressVPN\expressvpnctl.exe" get region
echo.

echo === current exit IP ===
powershell -NoProfile -Command "try { (Invoke-WebRequest -Uri 'https://api.ipify.org' -UseBasicParsing -TimeoutSec 10).Content } catch { 'probe failed' }"
echo.

echo === live rotation test (disconnects + reconnects to another country) ===
set VPN=1
go test -v -run TestLiveVPNRotation -timeout 5m .
set VPN=

echo.
echo === exit IP after rotation ===
powershell -NoProfile -Command "try { (Invoke-WebRequest -Uri 'https://api.ipify.org' -UseBasicParsing -TimeoutSec 10).Content } catch { 'probe failed' }"
echo.
pause