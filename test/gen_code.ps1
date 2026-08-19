<#
.SYNOPSIS
    test/ 目录 proto 代码生成脚本（仅 Python）
.DESCRIPTION
    从 test/api/agent.proto（契约快照）生成 Python stub 到 test/py/：
      - agent_pb2.py / agent_pb2_grpc.py
    供 runner.py（纯 gRPC 客户端）使用。

    注意：
      - Go 侧生成代码不属于本目录，按 PLATFORM-REFACTOR.md §7.1
        在项目 api/ 下生成（随 sidecar 实现一并落地）。
      - proto 事实源为 docs/PLATFORM-API.md；test/api/agent.proto 为同步快照。
#>
param(
    [string]$Python = "python"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

New-Item -ItemType Directory -Force -Path "py" | Out-Null

Write-Host "-> generating Python code (test/py)" -ForegroundColor Cyan
& $Python -m grpc_tools.protoc `
    -I "$root\api" `
    --python_out=py --grpc_python_out=py `
    "$root\api\agent.proto"
if ($LASTEXITCODE -ne 0) { throw "Python proto generation failed" }

Write-Host "Done." -ForegroundColor Green
