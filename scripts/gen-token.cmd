@echo off
rem gen-token.cmd - wrapper for gen-freebuff-token.ps1 with ExecutionPolicy Bypass
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0gen-freebuff-token.ps1" %*
rem Ctrl+C stop (STATUS_CONTROL_C_EXIT 0xC000013A) is a clean exit, not an error
if "%ERRORLEVEL%"=="3221225786" exit /b 0
if "%ERRORLEVEL%"=="-1073741510" exit /b 0
if %ERRORLEVEL% NEQ 0 (
    echo.
    echo [ERROR] Token generator stopped with exit code %ERRORLEVEL%.
    pause
)
