# test/ — Agent 平台化改造的测试目录

> 权威契约见 **[`testcases.md`](./testcases.md)**（冻结用例清单 + 文档覆盖映射矩阵）。

本目录承载两个目标：

- **目标1（自动化验证）**：`runner.py` 是模拟调用方（平台侧），通过本地 gRPC 与被测
  sidecar（`cmd/agentd`）通信，**与项目代码零依赖**（不 import 任何包，仅依赖 `grpcio`
  与生成的 Python stub）。
- **目标2（浏览器端真实验证）**：`agentdrv/` 是**模拟平台的独立程序**（测试步骤，非业务
  代码）：沿用改造前逻辑读 `agent.yaml`，经 gRPC 把配置注入并驱动 sidecar 实现改造前
  全部功能，再用浏览器打开 `/app/` 人工验证 sidecar 真实可用（覆盖 runner 自动化用例
  触达不到的完整 WebRTC 链路）。

## 目录结构

| 文件/目录 | 说明 |
|------|------|
| `testcases.md` | **冻结的测试用例契约**（唯一权威，含文档→用例覆盖映射矩阵） |
| `runner.py` | 目标1：用例加载器（gRPC 客户端 + Python 以浏览器身份连 signaling 模拟 bound/unbound） |
| `agentdrv/` | 目标2：模拟平台 driver（Go 程序，读 `agent.yaml` → `Init`/`Start`/`PrepareDevice` 驱动 sidecar） |
| `api/agent.proto` | gRPC 契约快照（与 `docs/GRPC-API.md` §2/§4 同步；以项目侧 `api/agent.proto` 为准） |
| `py/` | 生成的 Python 代码（`agent_pb2.py` / `agent_pb2_grpc.py`） |
| `gen_code.ps1` | 仅生成 Python stub（旧脚本；统一用 `scripts/gen-proto.ps1`） |
| `requirements.txt` | Python 依赖（grpcio / grpcio-tools / websockets） |

> 说明：Go 侧生成代码（`api/gen/`）不属于本目录 —— 它供项目侧 sidecar 实现使用，
> 按 `PLATFORM-REFACTOR.md` §7.1 落在项目 `api/` 下，随 sidecar 实现一并生成。

## 准备

```powershell
pip install -r requirements.txt
..\scripts\gen-proto.ps1   # 生成 api/gen（Go）+ test/py（Python），见脚本头部
```

## 运行

被测对象是**项目侧 sidecar**（`cmd/agentd`）：

```powershell
# 1) 启动 signaling（连通性/会话用例需要）
go run ./cmd/signaling -c ./config/signaling.yaml

# 2) 启动 sidecar（项目侧交付物）
agentd --grpc-port 17890

# 3) 运行目标1 用例加载器
python runner.py                          # 自动化用例（状态机/设备/会话/事件流）
python runner.py --adb 127.0.0.1:16384    # 加连通性用例（真实 adb 设备）
python runner.py --case reset_device      # 单用例
```

### 目标2：浏览器端真实验证（agentdrv）

```powershell
# 前置：signaling + agentd 已启动（见上），模拟器已 adb 在线
# agentdrv：读 config/agent.yaml → Init/Start/PrepareDevice 驱动 sidecar
go run ./test/agentdrv -c ./config/agent.yaml --grpc 127.0.0.1:17890 --events

# 或让 agentdrv 自动拉起 agentd 子进程（模拟平台以子进程托管 sidecar）
go run ./test/agentdrv -c ./config/agent.yaml --agentd build/test/agentd.exe --events
```

agentdrv 保持前台运行（订阅事件流）；随后浏览器打开
`http://127.0.0.1:8080/app/?s=demo-service&d=device-1`，人工验证：
WebRTC 协商成功、画面出现、触控/画质切换/断开等改造前功能（对应 testcases M 组）。
Ctrl+C 后 agentdrv 会调用 `Stop()` 优雅收尾。

## 用例范围

用例按 `testcases.md` 执行：

- **自动化**：生命周期状态机（`Init`/`Start`/`Stop`/`ERR_NOT_INIT`/`ERR_ALREADY_INIT`）、
  设备管理（`PrepareDevice`/`ReleaseDevice`/`ResetDevice` 含 `configured` 字段）、
  会话生命周期（Python 连 signaling 模拟 bound/unbound）、管理干预（`ForceCloseSession`/`SetQuality`/`SendControl`）、
  事件流（多订阅/背压/断线不重放/信令故障 `AgentError`）、连通性。
- **人工/浏览器端**：完整 WebRTC 协商、QoS 数据真实性、`StreamDead`、`ERR_ACQUIRE_FAILED`。
- **明确不做**：sidecar 测试钩子、Playwright 浏览器自动化。

修改用例时**先改 `testcases.md` 再同步 `runner.py`**，保证契约先行。
