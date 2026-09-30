@echo off
title Agentd Web Dashboard
echo ======================================================
echo    Agentd - Autonomous Observation & Repair Daemon
echo ======================================================
echo.
echo Initializing local database if not already present...
"%~dp0agentd.exe" init
echo.
echo Launching Web Dashboard in your default browser...
start http://127.0.0.1:8080/
echo.
echo Starting Agentd background server on http://127.0.0.1:8080/
echo (Keep this window open while using the dashboard)
echo Press Ctrl+C to stop the server.
echo.
"%~dp0agentd.exe" serve
pause
