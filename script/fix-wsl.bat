@echo off
chcp 65001 >nul
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo 正在请求管理员权限以重置 WSL 服务...
    powershell -Command "Start-Process '%~f0' -Verb RunAs"
    exit /b
)

echo ===================================================
echo 正在强制结束卡死在 STOP_PENDING 状态的 WSLService...
echo ===================================================
taskkill /F /IM wslservice.exe /T >nul 2>&1
taskkill /F /IM wslinstaller.exe /T >nul 2>&1
taskkill /F /IM wsl.exe /T >nul 2>&1

echo.
echo 等待服务重置...
timeout /t 2 /nobreak >nul

echo ===================================================
echo 正在启动并测试 WSL...
echo ===================================================
wsl echo "[WSL OK] WSL 子系统已成功恢复正常运行！"

echo.
echo ===================================================
echo 正在 WSL 内部执行 Git Push 到 main...
echo ===================================================
wsl git -C "/mnt/c/Users/ubuntu/Documents/agentreach" remote set-url origin git@github.com:wangxumarshall/agentreach.git
wsl git -C "/mnt/c/Users/ubuntu/Documents/agentreach" push origin main

echo.
echo ===================================================
echo 全部执行完毕！
echo ===================================================
pause
