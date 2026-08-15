<#
.SYNOPSIS
    scrcpy-webrtc-remote - 一键构建 & 打包脚本

.DESCRIPTION
    编译两个模块并打包到 build/ 目录:
      build/signaling/  - 信令服务器 (云端, 含前端静态资源)
      build/agent/      - 设备 agent   (电脑端, 含 scrcpy-server.jar)
    每个目录包含: exe (Windows + Linux) + 部署配置。
    本脚本只构建 signaling 与 agent —— 与当前发布范围一致。

    运行时（部署目录内）:
      signaling.exe -c signaling.yaml     # 前端在 ./static (yaml 已生成好)
      agent.exe -c agent.yaml             # jar 在同目录, yaml 已生成好

.PARAMETER Clean
    编译前清理 Go 缓存

.PARAMETER SkipAgent
    跳过 agent 模块

.PARAMETER SkipSignal
    跳过 signaling 模块

.PARAMETER WindowsOnly
    只构建 Windows 二进制（跳过 Linux 交叉编译）
#>
param(
    [switch]$Clean,
    [switch]$SkipAgent,
    [switch]$SkipSignal,
    [switch]$WindowsOnly
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

Write-Host "========================================" -ForegroundColor Cyan
Write-Host " scrcpy-webrtc-remote - Build Pipeline" -ForegroundColor Cyan
Write-Host " (signaling + agent only)" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

# ---- 1. Go environment ----
Write-Host "[1/5] Setting up Go environment..." -ForegroundColor Yellow
$useXxxModule = 'D:\usexxx\use-xxx.psm1'
$useGoLegacy = 'D:\go\use-go.ps1'
if (Get-Command go -ErrorAction SilentlyContinue) {
    # PATH 已有 go
} elseif (Test-Path $useXxxModule) {
    try {
        Import-Module $useXxxModule -Force -ErrorAction Stop
        use-go 1.22.5 | Out-Null
    } catch { }
} elseif (Test-Path $useGoLegacy) {
    try {
        . $useGoLegacy 1.22.7 | Out-Null
    } catch { }
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'go 不在 PATH。请安装 Go 1.22+（D:\usexxx 或 D:\go\use-go.ps1）'
}
if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }
go version | Write-Host

if ($Clean) {
    Write-Host "Cleaning Go cache..."
    go clean -cache
}

# ---- 2. Build (signaling + agent) ----
Write-Host ""
Write-Host "[2/5] Building modules..." -ForegroundColor Yellow

function Build-Module($name, $srcPath, $exeName) {
    Write-Host "  -> $name ... " -NoNewline
    $outPath = Join-Path $root "build\$name\$exeName"
    $buildOut = go build -o $outPath $srcPath 2>&1
    if ($LASTEXITCODE -eq 0) {
        $size = "{0:N1} MB" -f ((Get-Item $outPath).Length / 1MB)
        Write-Host "OK ($size)" -ForegroundColor Green
    } else {
        Write-Host "FAILED" -ForegroundColor Red
        $buildOut | ForEach-Object { Write-Host "    $_" -ForegroundColor Red }
        throw "Build failed for $name"
    }
}

$env:GOOS   = "windows"
$env:GOARCH = "amd64"

if (-not $SkipSignal) {
    Build-Module "signaling" "./cmd/signaling/" "signaling.exe"
    if (-not $WindowsOnly) {
        Write-Host "  -> signaling (linux) ... " -NoNewline
        $env:GOOS = "linux"
        $outPath = Join-Path $root "build\signaling\signaling_linux"
        go build -o $outPath "./cmd/signaling/" 2>&1 | Out-Null
        if ($LASTEXITCODE -eq 0) {
            $size = "{0:N1} MB" -f ((Get-Item $outPath).Length / 1MB)
            Write-Host "OK ($size)" -ForegroundColor Green
        } else {
            Write-Host "FAILED" -ForegroundColor Red
        }
        $env:GOOS = "windows"
    }
}
if (-not $SkipAgent) {
    Build-Module "agent" "./cmd/agent/" "agent.exe"
    if (-not $WindowsOnly) {
        Write-Host "  -> agent (linux) ... " -NoNewline
        $env:GOOS = "linux"
        $outPath = Join-Path $root "build\agent\agent_linux"
        go build -o $outPath "./cmd/agent/" 2>&1 | Out-Null
        if ($LASTEXITCODE -eq 0) {
            $size = "{0:N1} MB" -f ((Get-Item $outPath).Length / 1MB)
            Write-Host "OK ($size)" -ForegroundColor Green
        } else {
            Write-Host "FAILED" -ForegroundColor Red
        }
        $env:GOOS = "windows"
    }
}

# ---- 3. Copy config, static & runtime deps ----
Write-Host ""
Write-Host "[3/5] Copying config & static files..." -ForegroundColor Yellow

# --- signaling: 生成部署配置 (static_dir=./static) + 复制前端 ---
if (-not $SkipSignal) {
    $sigDir = Join-Path $root "build\signaling"
    $sigCfg = Join-Path $sigDir "signaling.yaml"
    $sigStatic = Join-Path $sigDir "static"
    $sigYaml = @"
# build.ps1 generated - deploy config for build/signaling/
host: "0.0.0.0"
port: 8080
static_dir: "./static"

webrtc:
  stun_servers:
    - "stun:stun.l.google.com:19302"
  turn_servers: []
"@
    Set-Content -Path $sigCfg -Value $sigYaml -Encoding UTF8
    Write-Host "  -> build/signaling/signaling.yaml (deploy config)"

    if (Test-Path "signaling/static") {
        if (-not (Test-Path $sigStatic)) { New-Item -ItemType Directory -Path $sigStatic -Force | Out-Null }
        Copy-Item "signaling/static/*" "$sigStatic/" -Force -Recurse
        Write-Host "  -> build/signaling/static/ (frontend)"
    }
}

# --- agent: 复制配置 + scrcpy-server.jar ---
if (-not $SkipAgent) {
    $agDir = Join-Path $root "build\agent"
    Copy-Item "config/agent.yaml" (Join-Path $agDir "agent.yaml") -Force
    Write-Host "  -> build/agent/agent.yaml (config)"

    if (Test-Path "scrcpy-server.jar") {
        Copy-Item "scrcpy-server.jar" (Join-Path $agDir "scrcpy-server.jar") -Force
        Write-Host "  -> build/agent/scrcpy-server.jar"
    } else {
        Write-Host "  ! 缺少 scrcpy-server.jar（agent 运行时需要）" -ForegroundColor Yellow
    }
}

# ---- 4/5. Summary ----
Write-Host ""
Write-Host "[5/5] Build complete!" -ForegroundColor Green
Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host " Output:" -ForegroundColor White
Write-Host "========================================" -ForegroundColor Cyan

Get-ChildItem "build" -Recurse |
    Where-Object { $_.PSIsContainer -eq $false -and $_.Name -match '\.(exe|yaml|jar|html|js|css)$|_linux$' } |
    Sort-Object FullName |
    ForEach-Object {
        $rel = ($_.FullName.Substring((Join-Path $root 'build\').Length)) -replace '\\', '/'
        $size = if ($_.Length) { "{0,7:N0} KB" -f ($_.Length / 1KB) } else { "" }
        Write-Host "  build/$rel $size"
    }

Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host " Deploy:" -ForegroundColor White
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  Cloud -> copy build/signaling/ to your cloud server, run:"
Write-Host "           signaling.exe -c signaling.yaml   (listen 0.0.0.0:8080, frontend at /app/)"
Write-Host "  PC    -> copy build/agent/ to your PC (with ADB), run:"
Write-Host "           agent.exe -c agent.yaml           (connect to signaling_url)"
Write-Host "========================================" -ForegroundColor Cyan
