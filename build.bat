@echo off
rem Builds OpenDM.exe (needs Go 1.22+ from https://go.dev/dl/)
set GOOS=windows
set GOARCH=amd64
go build -trimpath -ldflags "-H=windowsgui -s -w" -o OpenDM.exe .
if %errorlevel% neq 0 exit /b %errorlevel%
echo Built OpenDM.exe
