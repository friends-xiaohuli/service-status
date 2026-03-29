@echo off
setlocal

set "APP_NAME=service-status"
set "RELEASE_DIR=release\service-status-win-x64"
set "GOCACHE=%CD%\.gocache"

echo Building %APP_NAME%.exe ...
go build -o "%APP_NAME%.exe" .
if errorlevel 1 (
  echo Build failed
  exit /b 1
)

echo Preparing %RELEASE_DIR% ...
if exist "%RELEASE_DIR%" rmdir /s /q "%RELEASE_DIR%"
mkdir "%RELEASE_DIR%"
mkdir "%RELEASE_DIR%\web"

copy /y "%APP_NAME%.exe" "%RELEASE_DIR%\%APP_NAME%.exe" > nul
copy /y "config.json" "%RELEASE_DIR%\config.json" > nul
copy /y "start.bat" "%RELEASE_DIR%\start.bat" > nul
copy /y "web\*" "%RELEASE_DIR%\web\" > nul

echo.
echo Release ready:
echo   %CD%\%RELEASE_DIR%
echo.
echo Copy this whole folder to another Windows server and run start.bat
