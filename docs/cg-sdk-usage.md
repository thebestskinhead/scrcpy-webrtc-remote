# cg-sdk.js 使用文档

> 云游戏 WebRTC 远程控制 SDK（引擎层，无界面）
> 版本：`CG.SDK_VERSION = '1.0.0'` ｜ 文件：`signaling/static/app/js/cg-sdk.js`

`cg-sdk.js` 是 scrcpy-webrtc-remote 的开源前端公共包，只包含「引擎」能力：WebSocket + WebRTC 信令与数据通道生命周期、`getStats` 轮询与弱网/卡死检测、触控/键盘/剪贴板输入与视口坐标映射、信令中继查询、以及一键接入的一体化门面。**它不包含任何界面**，所有内部状态通过 `CG.events` 事件总线暴露，由页面层自行决定如何渲染。

---

## 目录

- [1. 设计原则](#1-设计原则)
- [2. 快速开始](#2-快速开始)
- [3. 两种接入方式](#3-两种接入方式)
- [4. API 参考](#4-api-参考)
  - [4.1 CG.events 事件总线](#41-cgevents-事件总线)
  - [4.2 CG.log / CG.toast](#42-cglog--cgtoast)
  - [4.3 CG.ConnectionManager](#43-cgconnectionmanager)
  - [4.4 CG.StatsMonitor](#44-cgstatsmonitor)
  - [4.5 CG.InputController](#45-cginputcontroller)
  - [4.6 CG.relayQuery](#46-cgrelayquery)
  - [4.7 CG.RemoteSession（一体化门面）](#47-cgremotesession一体化门面)
- [5. 事件参考](#5-事件参考)
- [6. 连接状态机](#6-连接状态机)
- [7. DataChannel 控制协议](#7-datachannel-控制协议)
- [8. 信令协议](#8-信令协议)
- [9. 重连与异常处理](#9-重连与异常处理)
- [10. 完整示例](#10-完整示例)

---

## 1. 设计原则

1. **纯引擎，不依赖任何具体 DOM 结构。** 需要挂视频/触摸层时通过构造参数注入（`videoEl` / `overlayEl`）；不传则回退到页面既有 id（`remoteVideo` / `controlOverlay` / `videoContainer` / `root`），保证「现有整页」零改动可用。
2. **所有状态通过 `CG.events` 暴露。** 页面层监听事件自行渲染，因此本包不含任何界面、HUD、开发者面板写入逻辑。
3. **只包含开源能力。** `perfmon` / `debugcollect` 等闭源调试能力由 dev 仓库在 SDK 之上注入，本包不感知（`debug_*` 相关方法在开源构建中为 no-op 或无人消费）。

---

## 2. 快速开始

```html
<script src="js/cg-sdk.js"></script>
```

SDK 以全局命名空间 `window.CG` 暴露。加载后即可使用：

```html
<video id="remoteVideo" autoplay muted playsinline></video>

<script>
  const conn = new CG.ConnectionManager();
  conn.connect('demo-service', 'device-1');
</script>
```

---

## 3. 两种接入方式

### 方式 A：整页默认（零改动）

页面中存在默认元素 id（`remoteVideo` 视频元素等），直接实例化 `CG.ConnectionManager` 并 `connect()`。参考 `app.js` 的编排方式。

### 方式 B：深度嵌入自有页面（推荐）

使用一体化门面 `CG.RemoteSession`，注入自有元素与信令地址：

```js
const session = new CG.RemoteSession({
  videoEl: document.getElementById('my-video'),
  overlayEl: document.getElementById('my-overlay'),
  signalingBase: 'cloud.example.com:8080',  // 默认用当前域
});

session.on('state-change', st => {
  console.log('state:', st);          // connected / reconnecting ...
});
session.on('stats', s => {
  hud.update({ loss: s.loss, rtt: s.rtt, fps: s.fps, bitrate: s.bitrate });
});
session.on('preempted', () => alert('设备被其他用户接管'));

session.connect('demo-service', 'device-1');
```

> `RemoteSession` 自动完成：连接管理 + 统计监控 + 输入绑定（触摸/键盘/剪贴板都接好了），并统一了重连策略。多数嵌入场景用这一个类就够了。

---

## 4. API 参考

### 4.1 CG.events 事件总线

SDK 内部与页面层共用同一个事件总线实例。

| 方法 | 说明 |
|------|------|
| `CG.events.on(evt, fn)` | 订阅事件 |
| `CG.events.off(evt, fn)` | 取消订阅 |
| `CG.events.emit(evt, data)` | 触发事件 |

完整事件清单见 [第 5 节](#5-事件参考)。

### 4.2 CG.log / CG.toast

SDK 的日志与提示出口，页面层可覆盖：

```js
CG.log   = msg => console.log('[CG] ' + msg);
CG.toast = (msg, type) => myUI.toast(msg, type);  // 默认 console
```

### 4.3 CG.ConnectionManager

`WebSocket + WebRTC` 信令/数据通道生命周期管理。

#### 构造参数 `new CG.ConnectionManager(opts)`

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `videoEl` | `HTMLVideoElement` | `null` | 注入视频元素（嵌入自有页面时）；否则回退到 `#remoteVideo` |
| `signalingBase` | `string` | `''` | 信令地址 `host:port`，空则用当前域（自动匹配 ws/wss） |
| `iceServers` | `RTCIceServer[]` | `null` | 自定义 ICE；不传则等待 agent 经信令下发 |
| `reconnect` | `{max, backoff[]}` | `null` | 重连策略（由 `RemoteSession` 使用） |

#### 连接 / 断开

| 方法 | 说明 |
|------|------|
| `async connect(svcId, instId)` | 建立会话。内部按序执行：WS 连接 → 等待 ICE 配置 → 创建 PeerConnection 并发起 offer |
| `disconnect(silent)` | 断开并清理。`silent=true` 时静默复位状态机，不发 `idle` 事件（重连路径用） |
| `get state()` | 当前状态机状态（见第 6 节） |
| `get isConnected()` | 是否处于 `connected` |

#### 控制指令（经 DataChannel "control" 发送）

| 方法 | 对应协议 |
|------|----------|
| `sendControl(type, data)` | 发送任意 `{type, data}` 控制消息 |
| `sendKeyEvent(action, kc)` | 按键注入，`action` 0=按下 1=抬起，`kc` 为 Android keycode |
| `sendText(text)` | 文本注入（可打印字符 / IME 上屏文本） |
| `sendTouch(action, pid, x, y, w, h, p, btns)` | 触摸注入（action: 0 down / 1 up / 2 move） |
| `sendScroll(x, y, w, h, hs, vs)` | 滚动注入（hscroll/vscroll 为滚动量） |
| `setClipboard(text, paste)` | 设置设备剪贴板；`paste=true` 时设备端同时触发一次粘贴 |
| `getClipboard()` | 请求读取设备剪贴板（设备经 DataChannel 回 `clipboard` 消息 → `device-clipboard` 事件） |
| `sendNotification()` | 展开通知面板 |
| `requestKeyframe()` | 请求关键帧（画面卡死时刷新） |

#### 调试能力（开源构建中无消费者，为 no-op）

`debugStart(durationMs)` / `debugStop()` / `debugFetch()` / `debugReset()` / `debugEnableTs()` —— 由闭源 `debugcollect` 组件消费。

### 4.4 CG.StatsMonitor

每 1 秒轮询 `pc.getStats()`，发出指标事件；检测弱网 / 连接类型 / 媒体停滞。

| 方法 | 说明 |
|------|------|
| `start(connMgr)` | 开始监控（传入 ConnectionManager） |
| `stop()` | 停止监控 |
| `setHudVisible(v)` | 是否写 HUD 元素（`#hudLoss` 等，缺失时静默跳过） |
| `videoFrameCount`（get/set） | 已渲染帧计数（页面层经 `requestVideoFrameCallback` 维护，随 `decoder_status` 上报） |

输出的关键事件见 [第 5 节](#5-事件参考) 的 `stats` / `connection-type` / `resolution` / `media-stalled` / `link-degraded`。

**媒体停滞判定**（判「卡死」不判「静止」）：码流仍在但 15 秒不出帧 → 触发 `media-stalled` 事件（隐藏页面时不检测）。

### 4.5 CG.InputController

触控/键盘/剪贴板输入 + 视口→设备坐标映射（支持方向锁定的旋转坐标换算）。

```js
const input = new CG.InputController();
input.bind(connMgr, {
  overlayEl: document.getElementById('my-overlay'),   // 触摸层
  videoEl: document.getElementById('my-video'),
  containerEl: document.getElementById('my-container'),
  rootEl: document.getElementById('my-root'),
});
// 元素均可选，缺省回退到默认 id
```

| 方法 | 说明 |
|------|------|
| `bind(connMgr, opts)` | 绑定连接与 DOM 元素，挂载 pointer/wheel/keyboard/IME/clipboard 监听 |
| `onResolutionChange(w, h)` | 分辨率变化时外部调用（一般由 `resolution` 事件驱动） |
| `resolution`（get/set） | 设备分辨率，坐标映射基准（默认 1080×1920，随视频元数据自动更新） |

行为约定：
- 触摸/滚动自动映射到设备坐标（`clientX/Y` → 设备坐标）。
- 功能键走 `inject_keycode`（Backspace / Enter / Tab / 方向键 / 音量 / Power 等），可打印字符走 `inject_text`，Ctrl/Meta/Alt 组合键不拦截。
- 中文、日文等 IME 组合输入在 `compositionend` 整段一次注入。
- 页面内 `input`/`textarea`/`contenteditable` 处于编辑态时（`_isLocalEditing`）不拦截键盘与剪贴板。
- 粘贴事件自动 `setClipboard(text, true)`（发送并触发设备端粘贴），并发 `clipboard-sent` 事件。

### 4.6 CG.relayQuery

信令中继查询：在不占用会话的前提下，临时开一条 relay 通道查询设备状态或发起抢占。

```js
// 查询设备是否被占用
const resp = await CG.relayQuery(svc, inst, { type: 'query_status' });
// resp.payload.busy === true 表示设备忙

// 发起抢占（接管设备）
await CG.relayQuery(svc, inst, { type: 'preempt' });
```

签名：`CG.relayQuery(svcId, instId, payload, opts) => Promise`，`opts.host` 可指定信令地址；5 秒超时。

典型使用流程（参考 `app.js` `_connect()`）：

```js
const resp = await CG.relayQuery(svc, inst, { type: 'query_status' });
if (resp.payload && resp.payload.busy) {
  const ok = await userConfirm('设备正被占用，接管？');
  if (!ok) return;
  await CG.relayQuery(svc, inst, { type: 'preempt' });
}
conn.connect(svc, inst);
```

### 4.7 CG.RemoteSession（一体化门面）

封装 `ConnectionManager + StatsMonitor + InputController`，对外只暴露 `on/off/connect/disconnect` + 控制透传。

#### 构造参数

| 参数 | 默认 | 说明 |
|------|------|------|
| `stats` | `true` | 设为 `false` 关闭统计监控 |
| `input` | `null` | 输入绑定元素配置（传给 `InputController.bind` 的 `opts`） |
| 其余 | — | 透传给 `ConnectionManager`（`videoEl` / `overlayEl` / `signalingBase` / `iceServers`） |

#### 方法

| 方法 | 说明 |
|------|------|
| `on(evt, fn)` / `off(evt, fn)` / `once(evt, fn)` | 事件订阅（链式） |
| `async connect(svc, inst)` | 连接（收到首个视频轨后自动启动统计 + 绑定输入） |
| `disconnect(silent)` | 断开 |
| `sendText(t)` / `sendKeyEvent(a, kc)` / `sendTouch(...)` / `sendScroll(...)` | 控制透传 |
| `setClipboard(t, paste)` / `getClipboard()` | 剪贴板透传 |
| `sendControl(act, p)` | 任意控制消息透传 |
| `requestKeyframe()` | 请求关键帧 |
| `setQuality(level)` | 切换画质：`clear` / `hd` / `original`（只切码率，分辨率不变） |

#### 属性

`isConnected` / `state` / `input` / `stats` / `connection`（底层 ConnectionManager 实例）。

---

## 5. 事件参考

所有事件经 `CG.events` 发出，`RemoteSession` 的 `on/off/once` 即转发此总线。

### 连接流程

| 事件 | 数据 | 说明 |
|------|------|------|
| `connecting` | `{svcId, instId}` | 开始连接 |
| `step` | `{id, state, detail?}` | 技术步骤进度。`id`: `ws` / `ice-cfg` / `sdp` / `ice` / `video` / `first`；`state`: `active` / `done` / `error`；`ice-cfg` done 时附 `detail`（STUN/TURN 列表） |
| `state-change` | 状态字符串 | 状态机变化（见第 6 节） |
| `track-video` | `MediaStreamTrack` | 首个视频轨到达（可在此 `video.play()`、启动统计） |
| `connection-type` | `{type, localType, remoteType, label}` | 连接类型判定：`relay`（TURN 中转）/ `p2p`（直连） |

### 运行中指标

| 事件 | 数据 | 说明 |
|------|------|------|
| `stats` | `{loss, rtt, fps, bitrate}` | 每秒网络指标（弱网判定依据，如 `loss>3%` 或 `rtt>150ms`） |
| `resolution` | `{width, height}` | 视频分辨率变化 → 用于坐标映射与旋转 |
| `link-degraded` | `{degraded}` | ICE 抖动（可恢复）提示，弱网徽标用 |
| `media-stalled` | — | 媒体停滞 15s（码流仍在、帧不出）→ 建议触发重连 |
| `device-clipboard` | `{text}` | 设备剪贴板主动上报 / `get_clipboard` 响应 |
| `clipboard-sent` | `{length}` | 粘贴事件已发送到设备 |

### 异常与生命周期

| 事件 | 数据 | 说明 |
|------|------|------|
| `error` | `{message, fatal?}` | 连接失败。`fatal` 为真表示不可恢复，需引导用户 |
| `lost` | `{reason}` | 会话中掉线。`reason`: `ws-close` / `stream-dead` / `ice-failed` / `pc-failed`。通常应进入自动重连 |
| `idle` | — | 普通断开（用户主动 / 未连接时 WS 关闭） |
| `preempted` | — | 设备被其他用户接管，SDK 已自动断开 |
| `debug-log` / `debug-chunk` | `{line}` / `{seq, total, chunk}` | 设备端调试日志（开源构建无消费者） |

---

## 6. 连接状态机

`ConnectionManager.state` / `RemoteSession.state` 取值：

| 状态 | 含义 |
|------|------|
| `idle` | 空闲 |
| `ws_connecting` | WebSocket 连接中（10s 超时） |
| `waiting_ice` | 等待 ICE 服务器配置（3s 超时后使用默认 STUN） |
| `ws_open` | WebSocket 已建立，创建 PeerConnection |
| `offer_sent` | SDP offer 已发出（20s 协商超时） |
| `answer_received` | 收到 answer，设置远端描述 |
| `ice_connecting` | ICE 连接中（30s 超时） |
| `connected` | 已连接（视频首帧播放后进入） |
| `error` | 出错终止（发 `error` 事件） |

连接旅程：`connecting → connected`；会话中掉线进入 `lost`（可自动重连）；被抢占进入 `preempted`。

---

## 7. DataChannel 控制协议

浏览器 → 设备（DataChannel 名为 `control`，消息为 JSON `{type, data}`）：

| type | data | 说明 |
|------|------|------|
| `inject_touch` | `{action, pointer_id, x, y, width, height, pressure, action_button, buttons}` | 触摸（action: 0 down / 1 up / 2 move） |
| `inject_scroll` | `{x, y, width, height, hscroll, vscroll, buttons}` | 滚动 |
| `inject_keycode` | `{action, keycode, repeat, metastate}` | 物理按键（Android keycode） |
| `inject_text` | `{text}` | 文本注入 |
| `back_or_screen_on` | `{action}` | 返回键 / 点亮屏幕 |
| `set_clipboard` | `{sequence, paste, text}` | 设置剪贴板（`paste=true` 触发粘贴） |
| `get_clipboard` | `{copy_key}` | 读取剪贴板 |
| `set_display_power` | `{on}` | 熄屏 / 亮屏 |
| `rotate_device` | `{}` | 旋转设备 |
| `collapse_panels` | `{}` | 收起面板 |
| `expand_notification`（别名 `inject_notification`） | `{}` | 展开通知面板 |
| `expand_settings` | `{}` | 展开设置面板 |
| `open_hard_keyboard_settings` | `{}` | 打开硬键盘设置 |
| `reset_video` | `{}` | 请求关键帧（卡死刷新） |
| `set_quality` | `{level}` | 画质：`clear` / `hd` / `original` |

设备 → 浏览器（DataChannel `control`，SDK 解析并转发事件）：

| type | data | SDK 事件 |
|------|------|----------|
| `clipboard` | `{text}` | `device-clipboard` |
| `debug_log` | `{data: {line}}` | `debug-log` |
| `debug_log_batch` | `{data: {seq, total, chunk}}` | `debug-chunk` |

---

## 8. 信令协议

浏览器信令地址：`/ws/browser/{service_id}/{instance_id}`（ws/wss 随页面协议自动切换）。

### 浏览器 → signaling

| type | 数据 | 说明 |
|------|------|------|
| `offer` | `{offer: {sdp, type}}` | SDP 协商 |
| `ice_candidate` | `{candidate: {candidate, sdpMid, sdpMLineIndex}}` | Trickle ICE |
| `first_frame` | `{data: {ts}}` | 首帧已渲染 |
| `decoder_status` | `{data: {decoded, dropped, pli, fir, keyFrames, rendered, size, fps, audioLost, rttMs, lossRate}}` | QoS 遥测（每 3s，驱动 agent 降码率与 FEC） |
| `decoder_stats` | `{data: {framesDecoded}}` | 解码帧数（每 2s） |
| `reset_video` | — | 请求关键帧 |

### signaling → 浏览器

| type | 说明 |
|------|------|
| `ice_servers` | ICE 服务器列表（agent 下发） |
| `answer` | SDP answer（附 `pc_id`） |
| `ice_candidate` | 对端 Trickle ICE |
| `error` | 信令错误（`message` 字段） |
| `stream_dead` | scrcpy 进程死亡（会触发自动重连） |
| `preempted` | 设备被接管 |

### relay 通道

地址：`/ws/relay/{service_id}/{instance_id}`。消息为 `{type: 'relay', relay_id, payload}`，`relay_id` 回显匹配响应。典型 payload：

- `query_status` → 响应含 `busy` 字段（占用检测）
- `preempt` → 抢占设备

---

## 9. 重连与异常处理

SDK 本身只发事件，重连策略由页面层决定。推荐做法（参考 `app.js`）：

1. 监听 `lost`（掉线）与 `media-stalled`（卡死）→ 进入自动重连。
2. 重连前先 `conn.disconnect(true)` 静默拆除残留连接（否则 `connect()` 的状态前置检查会拒绝）。
3. 指数退避（如 `[1.5, 2, 3, 4, 5]` 秒），上限次数（如 5 次），耗尽后展示失败页。
4. 监听 `error`：重连过程中失败 → 继续调度下一次；首连失败 → 翻译错误文案引导用户。
5. 监听 `preempted`：提示用户设备被接管，回到入口页。

错误信息关键词速查：`ICE|NAT|TURN` → NAT/防火墙限制直连；`SDP|信令` → 设备无响应；其余 → 网络不可达。

---

## 10. 完整示例

### 最小可玩示例（零构建、零依赖）

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>云游戏 SDK Demo</title>
  <style>
    #wrap { max-width: 480px; margin: 0 auto; position: relative; }
    video { width: 100%; aspect-ratio: 9/19.5; background: #000; }
    #overlay { position: absolute; border: 1px dashed rgba(255,255,255,.3); }
  </style>
</head>
<body>
  <div id="wrap">
    <video id="remoteVideo" autoplay playsinline muted></video>
    <div id="controlOverlay"></div>
  </div>

  <script src="js/cg-sdk.js"></script>
  <script>
    CG.log = msg => console.log('[CG]', msg);

    const session = new CG.RemoteSession({
      videoEl: document.getElementById('remoteVideo'),
      overlayEl: document.getElementById('controlOverlay'),
      containerEl: document.getElementById('wrap'),
    });

    const statusEl = document.createElement('div');
    document.body.appendChild(statusEl);

    session.on('state-change', st => { statusEl.textContent = '状态: ' + st; });
    session.on('error', e => alert('连接失败: ' + e.message));
    session.on('preempted', () => alert('设备被其他用户接管'));
    session.on('stats', s => {
      document.title = `${s.fps}fps ${s.bitrate}kbps 丢包${s.loss}%`;
    });

    // 从 URL 读取 s / d 参数，或用固定值
    const p = new URLSearchParams(location.search);
    session.connect(p.get('s') || 'demo-service', p.get('d') || 'device-1');
  </script>
</body>
</html>
```

### 控制透传示例

```js
// 按键：返回（Android keycode 4）
session.sendKeyEvent(0, 4);            // DOWN
setTimeout(() => session.sendKeyEvent(1, 4), 60);  // UP

// 文本（中文 / IME 上屏）
session.sendText('hello 世界');

// 剪贴板：发送到设备并粘贴
session.setClipboard('内容', true);

// 读取设备剪贴板 → 监听 device-clipboard
session.getClipboard();
session.on('device-clipboard', d => console.log('设备剪贴板:', d.text));

// 画质切换
session.setQuality('hd');   // clear | hd | original
```

---

## 附录：相关文件

| 文件 | 说明 |
|------|------|
| `signaling/static/app/js/cg-sdk.js` | SDK 本体 |
| `signaling/static/app/js/app.js` | 官方整页编排示例（事件接线、自动重连、错误翻译的参考实现） |
| `README.md` | 项目总览、信令协议速览 |
