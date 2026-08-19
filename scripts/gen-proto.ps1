<#
.SYNOPSIS
    从 api/agent.proto 生成 Go（api/gen）与 Python（test/py）代码
.DESCRIPTION
    依赖：
      - protoc-gen-go / protoc-gen-go-grpc（Go 插件，安装见 README 或下方注释）
      - Python grpcio-tools（提供 protoc 可执行环境，经 grpc_tools.protoc 调用）

    用法：
      .\scripts\gen-proto.ps1
        默认使用 PATH 中的 protoc-gen-go 与 python 解释器
      .\scripts\gen-proto.ps1 -Python D:\usexxx\python\versions\3.11.9\python.exe
        指定 python 解释器（其 site-packages 需含 grpcio-tools）
      .\scripts\gen-proto.ps1 -GoPlugin D:\usexxx\gopath\bin\protoc-gen-go.exe -GoGrpcPlugin D:\usexxx\gopath\bin\protoc-gen-go-grpc.exe
        指定 Go 插件路径（未指定则查找 PATH）

    安装 Go 插件（需联网，D:\usexxx 环境示例）：
      $env:GOROOT='D:\usexxx\go\versions\1.22.5\go'
      $env:GOPATH='D:\usexxx\gopath'
      & $env:GOROOT\bin\go.exe install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
      & $env:GOROOT\bin\go.exe install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
#>
param(
    [string]$Python = "python",
    [string]$GoPlugin = "",      # protoc-gen-go 可执行文件
    [string]$GoGrpcPlugin = ""   # protoc-gen-go-grpc 可执行文件
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path   # scripts/
$repo = Split-Path -Parent $root                          # repo root

# 1) 定位 Go 插件
if (-not $GoPlugin) { $GoPlugin = (Get-Command protoc-gen-go -ErrorAction SilentlyContinue).Source }
if (-not $GoGrpcPlugin) { $GoGrpcPlugin = (Get-Command protoc-gen-go-grpc -ErrorAction SilentlyContinue).Source }
if (-not $GoPlugin -or -not $GoGrpcPlugin) {
    throw "protoc-gen-go / protoc-gen-go-grpc 未找到。请先 go install（见脚本头部注释）或显式传 -GoPlugin / -GoGrpcPlugin。"
}

# 2) 准备输出目录
New-Item -ItemType Directory -Force -Path "$repo\api\gen" | Out-Null
New-Item -ItemType Directory -Force -Path "$repo\test\py" | Out-Null

# 3) 生成 Go 代码（api/gen，package agentapi）
Write-Host "-> generating Go code (api/gen)" -ForegroundColor Cyan
Push-Location $repo
try {
    & $Python -m grpc_tools.protoc `
        -I "$repo\api" `
        --plugin "protoc-gen-go=$GoPlugin" `
        --plugin "protoc-gen-go-grpc=$GoGrpcPlugin" `
        --go_out "$repo\api\gen" --go_opt paths=source_relative `
        --go-grpc_out "$repo\api\gen" --go-grpc_opt paths=source_relative `
        "$repo\api\agent.proto"
    if ($LASTEXITCODE -ne 0) { throw "Go proto generation failed" }
} finally {
    Pop-Location
}

# 4) 生成 Python 代码（test/py）
Write-Host "-> generating Python code (test/py)" -ForegroundColor Cyan
Push-Location $repo
try {
    & $Python -m grpc_tools.protoc `
        -I "$repo\api" `
        --python_out "$repo\test\py" --grpc_python_out "$repo\test\py" `
        "$repo\api\agent.proto"
    if ($LASTEXITCODE -ne 0) { throw "Python proto generation failed" }
} finally {
    Pop-Location
}

Write-Host "Done." -ForegroundColor Green
