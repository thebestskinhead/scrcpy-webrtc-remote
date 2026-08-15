<#
.SYNOPSIS
    Scrcpy WebRTC Remote - 开发调试脚本（go run 模式: signaling + agent）

.DESCRIPTION
    面向本仓库 agent + signaling 部署形态的日常调试工具。所有组件都用
    `go run` 直接运行源码，不预编译任何二进制：

      start   以 go run 启动 signaling + agent（自动 adb connect 模拟器，日志落盘）
      stop    按 PID 文件停止两个 go run 进程树
      status  检查进程/健康接口/agent 注册/adb 设备/端口池占用
      monitor 循环监控: 健康、agent 注册、端口池、日志关键告警
      all     start + status

    go run 说明:
      - 首次启动会编译两个命令（含依赖下载），耗时明显长于热启动；
        编译产物由 go 缓存在临时目录，进程退出后自动清理，仓库内不留二进制。
      - start 记录的 PID 是 cmd 包装进程，stop 用 taskkill /T 连 go run
        子进程（真正的 signaling/agent 进程）一起终止。

.PARAMETER Action
    start | stop | status | monitor | all (默认 all)

.PARAMETER AdbPath
    adb.exe 路径。默认自动探测: 先 PATH，再常见 MuMu 安装路径。

.PARAMETER AdbHost
    adb 连接地址。默认 127.0.0.1:16384 (MuMu)

.PARAMETER DeviceSerial
    agent.yaml 使用的设备 serial，默认取 -AdbHost

.PARAMETER ServiceId
    服务 ID，浏览器/agent 共用。默认 demo-service

.PARAMETER InstanceId
    实例 ID（设备）。默认 device-1

.PARAMETER WebPort
    signaling 监听端口。默认 8080

.PARAMETER LogDir
    日志输出目录。默认 <repo>\logs\dev

.PARAMETER DurationSec
    monitor 持续秒数；0 = 直到 Ctrl+C。默认 0

.PARAMETER AudioEnabled
    是否启用音频采集。默认关闭——MuMu 模拟器镜像通常没有
    audio/opus MediaCodec 编码器，开启会导致 scrcpy server 崩溃；
    真机/支持 opus 的环境可用 -AudioEnabled 打开。
#>
[CmdletBinding()]
param(
    [ValidateSet('start', 'stop', 'status', 'monitor', 'all')]
    [string]$Action = 'all',
    [string]$AdbPath = '',
    [string]$AdbHost = '127.0.0.1:16384',
    [string]$DeviceSerial = '',
    [string]$ServiceId = 'demo-service',
    [string]$InstanceId = 'device-1',
    [int]$WebPort = 8080,
    [string]$LogDir = '',
    [int]$DurationSec = 0,
    [switch]$AudioEnabled
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# ============================================================
# 路径与环境
# ============================================================
$script:root     = Split-Path -Parent $MyInvocation.MyCommand.Path
if ([string]::IsNullOrWhiteSpace($LogDir)) {
    $script:logDir = Join-Path $root 'logs\dev'
} else {
    $script:logDir = $LogDir
}
if ([string]::IsNullOrWhiteSpace($DeviceSerial)) { $DeviceSerial = $AdbHost }

$script:cfgDir        = Join-Path $logDir 'config'
$script:signalingPid  = Join-Path $logDir 'signaling.pid'
$script:agentPid      = Join-Path $logDir 'agent.pid'
$script:signalingLog  = Join-Path $logDir 'signaling.log'
$script:agentLog      = Join-Path $logDir 'agent.log'
$script:wsCfg         = Join-Path $cfgDir 'signaling.yaml'
$script:agCfg         = Join-Path $cfgDir 'agent.yaml'
$script:baseUrl       = "http://127.0.0.1:$WebPort"

function Info([string]$m) { Write-Host "==> $m" }
function Ok  ([string]$m) { Write-Host "  $([char]0x2713) $m" -ForegroundColor Green }
function Warn([string]$m) { Write-Host "  ! $m" -ForegroundColor Yellow }
function Err_([string]$m) { Write-Host "  $([char]0x2717) $m" -ForegroundColor Red }
function Step([string]$m) { Write-Host ""; Write-Host "==> $m" -ForegroundColor Cyan }

function Ensure-Dir([string]$p) { if (-not (Test-Path $p)) { New-Item -ItemType Directory -Path $p -Force | Out-Null } }

# ============================================================
# Go 工具链：PATH → D:\usexxx → D:\go\use-go.ps1
# ============================================================
function Ensure-Go {
    if (Get-Command go -ErrorAction SilentlyContinue) {
        go version | ForEach-Object { Ok "go: $_" }
        return
    }
    # 本机私有工具链加载器（可选；没有就要求 PATH 里有 go）
    $useXxxModule = 'D:\usexxx\use-xxx.psm1'
    $useGoLegacy = 'D:\go\use-go.ps1'
    if (Test-Path $useXxxModule) {
        try {
            Import-Module $useXxxModule -Force -ErrorAction Stop
            use-go 1.22.5 | Out-Null
            if (Get-Command go -ErrorAction SilentlyContinue) {
                go version | ForEach-Object { Ok "go: $_ (via use-xxx)" }
                return
            }
        } catch { }
    }
    if (Test-Path $useGoLegacy) {
        try {
            . $useGoLegacy 1.22.7 | Out-Null
            if (Get-Command go -ErrorAction SilentlyContinue) {
                go version | ForEach-Object { Ok "go: $_ (via use-go.ps1)" }
                return
            }
        } catch { }
    }
    throw '未找到 go。请安装 Go 1.22+ 并加入 PATH（或提供 D:\usexxx / D:\go\use-go.ps1）'
}

# ============================================================
# adb：参数指定 → PATH → 常见 MuMu 安装路径
# ============================================================
function Resolve-Adb {
    if (-not [string]::IsNullOrWhiteSpace($AdbPath)) {
        if (-not (Test-Path $AdbPath)) { throw "adb not found: $AdbPath" }
        return (Resolve-Path $AdbPath).Path
    }
    $cmd = Get-Command adb -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($cand in @(
        'D:\Program Files\NetEase\MuMu\nx_main\adb.exe',
        'D:\Program Files\Netease\MuMu Player 12\shell\adb.exe',
        "$env:ProgramFiles\MuMu\emulator\nemu\vmonitor\bin\adb_server.exe"
    )) {
        if (Test-Path $cand) { return $cand }
    }
    throw '未找到 adb.exe。请用 -AdbPath 指定 adb 路径'
}

# ============================================================
# 生成运行时配置（替换设备 serial / 端口 / 音频）
# ============================================================
function Write-Configs {
    Ensure-Dir $cfgDir

    $staticFwd = (Join-Path $root 'signaling\static') -replace '\\', '/'
    $sig = @"
# dev.ps1 generated - signaling config
host: "127.0.0.1"
port: $WebPort
static_dir: "$staticFwd"

webrtc:
  stun_servers:
    - "stun:stun.l.google.com:19302"
  turn_servers: []
"@
    Set-Content -Path $wsCfg -Value $sig -Encoding UTF8
    Ok "wrote $wsCfg (port=$WebPort, static=$staticFwd)"

    $audioYaml = if ($AudioEnabled) { 'true' } else { 'false' }
    $ag = @"
# dev.ps1 generated - agent config
signaling_url: "ws://127.0.0.1:$WebPort"
service_id: "$ServiceId"

instances:
  - instance_id: "$InstanceId"
    device_serial: "$DeviceSerial"

scrcpy:
  server_version: "4.0"
  jar_path: "./scrcpy-server.jar"
  port_pool_start: 30000
  port_pool_size: 100
  max_size: 1920
  video_bit_rate: 12000000
  min_video_bit_rate: 300000
  clear_bitrate: 3000000
  hd_bitrate: 6000000
  warm_keep_seconds: 300
  audio_bit_rate: 256000
  video_codec: "h264"
  audio_codec: "opus"
  audio_enabled: $audioYaml
  power_on: true
  stay_awake: true
  video_keyframe_interval: 2
"@
    Set-Content -Path $agCfg -Value $ag -Encoding UTF8
    Ok "wrote $agCfg (serial=$DeviceSerial, svc=$ServiceId)"
}

# ============================================================
# 进程管理（go run）
# ============================================================
function Test-PortFree([int]$port) {
    $c = Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue
    return ($null -eq $c)
}

function Start-GoRunProcess {
    param(
        [string]$Package,          # e.g. ./cmd/signaling
        [string]$ConfigFile,       # -c 参数
        [string]$LogFile,
        [string]$PidFile
    )
    Ensure-Dir (Split-Path -Parent $LogFile)
    Ensure-Dir (Split-Path -Parent $PidFile)
    # cmd /c 包装: stdout+stderr 重定向到同一日志（go run 的编译输出也一并落盘）；
    # 记录的是 cmd 包装进程 PID，stop 时用 taskkill /T 连 go run 及其子进程一起杀。
    $argStr = (@('run', $Package, '-c', "`"$ConfigFile`"") -join ' ')
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = 'cmd.exe'
    $psi.Arguments = "/c `"go $argStr > `"$LogFile`" 2>&1`""
    $psi.WorkingDirectory = $root
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $proc = [System.Diagnostics.Process]::Start($psi)
    Set-Content -Path $PidFile -Value $proc.Id
    return $proc
}

function Stop-ByPidFile([string]$pidFile, [string]$name) {
    if (Test-Path $pidFile) {
        $procId = [int](Get-Content $pidFile -Raw).Trim()
        $p = Get-Process -Id $procId -ErrorAction SilentlyContinue
        if ($p) {
            # PID 是 cmd 包装进程，/T 连子进程(go run → signaling/agent)一起杀
            taskkill.exe /PID $procId /T /F 2>&1 | Out-Null
            Start-Sleep -Milliseconds 300
            $still = Get-Process -Id $procId -ErrorAction SilentlyContinue
            if (-not $still) { Ok "stopped $name (pid $procId)" } else { Warn "stop $name 未完全退出" }
        } else {
            Ok "$name 已不在运行 (pid $procId)"
        }
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    } else {
        Warn "no pid file for $name"
    }
}

# 兜底: 按命令行特征清理本仓库残留的 go run 进程（PID 文件丢失等场景）。
# 特征: 命令行包含本仓库 runtime 配置目录，或包含 go run ./cmd/xxx。
function Stop-StrayProcesses {
    $cfgEsc = [regex]::Escape($cfgDir)
    $rootEsc = [regex]::Escape($root)
    Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
        Where-Object {
            $cl = $_.CommandLine
            if (-not $cl) { return $false }
            if ($cl -match $cfgEsc) { return $true }
            if ($cl -match $rootEsc -and $cl -match 'cmd[/\\](signaling|agent)') { return $true }
            return $false
        } |
        ForEach-Object {
            taskkill.exe /PID $_.ProcessId /T /F 2>&1 | Out-Null
            Ok "force stopped stray pid=$($_.ProcessId) ($($_.Name))"
        }
}

# ============================================================
# start
# ============================================================
function Invoke-Start {
    Step "Start (go run: signaling :$WebPort + agent $DeviceSerial)"

    Ensure-Go
    $adbPath = Resolve-Adb
    $adbDir = Split-Path -Parent $adbPath

    # 前置检查
    if (-not (Test-Path (Join-Path $root 'scrcpy-server.jar'))) {
        throw "缺少 scrcpy-server.jar（放在仓库根目录）"
    }
    if (-not (Test-PortFree $WebPort)) {
        $owner = (Get-NetTCPConnection -State Listen -LocalPort $WebPort -ErrorAction SilentlyContinue | Select-Object -First 1).OwningProcess
        throw "port $WebPort 已被占用 (PID $owner)。请先停止占用进程或改 -WebPort"
    }
    Ensure-Dir $logDir

    # adb 连接模拟器
    Info 'adb connect emulator...'
    & $adbPath connect $AdbHost 2>&1 | ForEach-Object { Write-Host "  $_" }
    $devices = & $adbPath devices 2>&1 | Out-String
    if ($devices -notmatch [regex]::Escape($AdbHost)) {
        Warn "device $AdbHost 不在 adb devices 列表中: $($devices -join '; ')"
    } else {
        Ok "adb device online: $AdbHost"
    }

    # 运行配置
    Write-Configs

    # 启动 signaling (go run)
    Info "go run ./cmd/signaling -> $signalingLog"
    Start-GoRunProcess -Package './cmd/signaling' -ConfigFile $wsCfg -LogFile $signalingLog -PidFile $signalingPid | Out-Null
    Start-Sleep -Seconds 2
    if (-not (Test-PortFree $WebPort)) { Ok "signaling listening on :$WebPort" } else { Warn 'signaling 可能未监听（看日志）' }

    # 启动 agent (go run) —— PATH 前置 adb 目录，agent 用 "adb" 命令
    Info "go run ./cmd/agent -> $agentLog (PATH+$adbDir)"
    $oldPath = $env:Path
    try {
        $env:Path = "$adbDir;$env:Path"
        Start-GoRunProcess -Package './cmd/agent' -ConfigFile $agCfg -LogFile $agentLog -PidFile $agentPid | Out-Null
    } finally {
        $env:Path = $oldPath
    }

    # 等待 agent 注册（首启含编译，给足时间）
    $deadline = (Get-Date).AddSeconds(120)
    $registered = $false
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 500
        if (Test-Path $agentLog) {
            $tail = Get-Content $agentLog -Tail 300 -ErrorAction SilentlyContinue | Out-String
            if ($tail -match 'agent instance connected|agent registered|received ICE servers') { $registered = $true; break }
        }
        if (-not (Test-Path $signalingPid)) { break }
    }
    if ($registered) { Ok 'agent registered at signaling' } else { Warn 'agent 未在 120s 内注册，检查 agent.log' }

    Ok "URL: $baseUrl  (service=$ServiceId, device=$InstanceId)"
}

# ============================================================
# stop / status
# ============================================================
function Invoke-Stop {
    Step 'Stop'
    Stop-ByPidFile $agentPid 'agent'
    Stop-ByPidFile $signalingPid 'signaling'
    Stop-StrayProcesses
}

function Invoke-Status {
    Step "Status (base $baseUrl)"
    foreach ($pair in @(
        @{ n = 'signaling'; pf = $signalingPid },
        @{ n = 'agent';     pf = $agentPid }
    )) {
        $running = $false
        if (Test-Path $pair.pf) {
            $procId = [int](Get-Content $pair.pf -Raw).Trim()
            $running = $null -ne (Get-Process -Id $procId -ErrorAction SilentlyContinue)
        }
        Write-Host ('  {0,-10} pidfile={1,-6} running={2}  (go run)' -f $pair.n, (Test-Path $pair.pf), $running)
    }

    Write-Host ''
    try {
        $h = Invoke-RestMethod -Uri "$baseUrl/api/health" -TimeoutSec 5
        Ok ("health: " + ($h | ConvertTo-Json -Compress))
    } catch { Warn "health 请求失败: $($_.Exception.Message)" }

    try {
        $a = Invoke-RestMethod -Uri "$baseUrl/api/agents" -TimeoutSec 5
        if ($a) {
            foreach ($ag in $a) { Ok ("agent: svc=$($ag.service_id) inst=$($ag.instance_id) serial=$($ag.device_serial) online=$($ag.online)") }
        } else { Warn 'no agents registered' }
    } catch { Warn "api/agents 请求失败: $($_.Exception.Message)" }

    Write-Host ''
    $adbPath = Resolve-Adb
    & $adbPath devices 2>&1 | ForEach-Object { Write-Host "  adb: $_" }

    Write-Host ''
    $pool = Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
        Where-Object { $_.LocalPort -ge 30000 -and $_.LocalPort -le 30099 } |
        Select-Object -ExpandProperty LocalPort -Unique
    if ($pool) { Write-Host "  端口池占用: $($pool -join ', ')" } else { Write-Host '  端口池占用: (无)' }
}

# ============================================================
# monitor — 循环监控
# ============================================================
function Invoke-Monitor {
    Step "Monitor (Ctrl+C 退出; 每 5s 一轮)"
    $stopAt = if ($DurationSec -gt 0) { (Get-Date).AddSeconds($DurationSec) } else { $null }
    $lastAgentSize = 0
    $lastSigSize = 0

    while ($true) {
        if ($stopAt -and (Get-Date) -gt $stopAt) { break }
        $ts = Get-Date -Format 'HH:mm:ss'
        Write-Host ''
        Write-Host "----- $ts -----" -ForegroundColor DarkCyan

        try {
            $h = Invoke-RestMethod -Uri "$baseUrl/api/health" -TimeoutSec 4
            $a = Invoke-RestMethod -Uri "$baseUrl/api/agents" -TimeoutSec 4
            $aStr = if ($a) { ($a | ForEach-Object { "$($_.instance_id):$($_.online)" }) -join ' ' } else { '(none)' }
            Write-Host ("  health ok | agents: {0}" -f $aStr)
        } catch { Write-Host "  health FAIL: $($_.Exception.Message)" -ForegroundColor Red }

        $adbPath = Resolve-Adb
        $dev = (& $adbPath devices 2>$null) -match 'device$'
        Write-Host "  adb devices: $($dev -join '; ')"

        $pool = Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
            Where-Object { $_.LocalPort -ge 30000 -and $_.LocalPort -le 30099 } |
            Select-Object -ExpandProperty LocalPort -Unique
        if ($pool) { Write-Host "  portpool: $($pool -join ',')" }

        foreach ($pair in @(
            @{ n = 'agent';     f = $agentLog;     last = [ref]$lastAgentSize },
            @{ n = 'signaling'; f = $signalingLog; last = [ref]$lastSigSize }
        )) {
            if (-not (Test-Path $pair.f)) { continue }
            $size = (Get-Item $pair.f).Length
            if ($size -gt $pair.last.Value) {
                $stream = [System.IO.File]::Open($pair.f, 'Open', 'Read', 'ReadWrite')
                try {
                    $stream.Seek($pair.last.Value, 'Begin') | Out-Null
                    $reader = [System.IO.StreamReader]::new($stream)
                    while (-not $reader.EndOfStream) {
                        $line = $reader.ReadLine()
                        if ($line) {
                            $clr = 'Gray'
                            if ($line -match 'WARN|ERROR|FAIL|panic|short buffer|ICE did not transition|PTS rollback|signaling connection lost|session cleanup|bitrate adjusted|FEC \(RED\)|PLI|state changed|browser bound|browser unbound|registered|first frame') {
                                $clr = 'Yellow'
                                if ($line -match 'ERROR|FAIL|panic|short buffer') { $clr = 'Red' }
                            }
                            Write-Host ("  [{0}] {1}" -f $pair.n, $line) -ForegroundColor $clr
                        }
                    }
                    $pair.last.Value = $size
                } finally { $reader.Dispose(); $stream.Dispose() }
            }
        }

        Start-Sleep -Seconds 5
    }
}

# ============================================================
# main
# ============================================================
Ensure-Dir $logDir

switch ($Action) {
    'start'   { Invoke-Start }
    'stop'    { Invoke-Stop }
    'status'  { Invoke-Status }
    'monitor' { Invoke-Monitor }
    'all'     {
        Invoke-Start
        Invoke-Status
    }
}

Write-Host ''
Write-Host "done. 日志目录: $logDir" -ForegroundColor Green
