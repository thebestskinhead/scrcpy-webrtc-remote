/**
 * cg-sdk.js — 云游戏 WebRTC 远程控制 SDK（引擎层，无界面）
 *
 * 这是开源公共包，仅包含「引擎」：
 *   - ConnectionManager  WebSocket + WebRTC 信令/数据通道生命周期
 *   - StatsMonitor       WebRTC getStats 轮询 + 弱网/卡死检测（只发事件）
 *   - InputController    触控/键盘/剪贴板输入 + 视口→设备坐标映射
 *   - relayQuery         信令中继查询（占用检测 / 抢占）
 *   - RemoteSession      一体化门面（on/off + connect/disconnect + 控制透传）
 *   - CG.events          事件总线（页面层可选监听）
 *
 * 设计原则：
 *   1. 纯引擎，不依赖任何具体 DOM 结构。需要挂视频/触摸层时通过构造参数注入
 *      （videoEl / overlayEl），不传则回退到当前页面既有 id（remoteVideo /
 *       controlOverlay / videoContainer / root），保证「现有整页」零改动可用。
 *   2. 所有内部状态通过 CG.events 暴露，页面层自行决定如何渲染 —— 因此本包
 *      不含任何界面、HUD、开发者面板写入逻辑（StatsMonitor 仅按需写入
 *      现有 id 的统计元素，缺失时静默跳过）。
 *   3. 公共包只包含开源能力；perfmon / debugcollect 等闭源调试能力由 dev 仓库
 *      在 SDK 之上注入，本包不感知。
 *
 * 协议（与 scrcpy-webrtc-remote 后端一致）：
 *   - 浏览器 WS：/ws/browser/{service_id}/{instance_id}
 *   - 信令消息：offer / answer / ice_candidate / ice_servers / error /
 *               preempted / stream_dead / first_frame / decoder_stats /
 *               decoder_status / reset_video
 *   - 控制通道：DataChannel "control"（{type, data}，inject_* / set_clipboard /
 *               get_clipboard / reset_video / set_quality / debug_*）
 *   - relay 查询：/ws/relay/{service_id}/{instance_id} + relay_id 包裹
 *
 * 用法（dev 普通整页）：
 *   <script src="js/cg-sdk.js"></script>
 *   const conn = new CG.ConnectionManager();
 *
 * 用法（dev 深度嵌入自有页面）：
 *   const s = new CG.RemoteSession({ videoEl, overlayEl, signalingBase });
 *   s.on('state-change', st => ...);
 *   s.connect(svc, inst);
 */
(function() {
  'use strict';

  const CG = (window.CG = window.CG || {});

  // ---------- 事件总线（页面层可在此基础上监听；SDK 也用同一实例） ----------
  CG.events = CG.events || (function() {
    const listeners = {};
    return {
      on(evt, fn)  { (listeners[evt] = listeners[evt] || []).push(fn); },
      off(evt, fn) { const arr = listeners[evt]; if (arr) { const i = arr.indexOf(fn); if (i >= 0) arr.splice(i, 1); } },
      emit(evt, d) { (listeners[evt] || []).forEach(fn => fn(d)); },
    };
  })();

  // ---------- 日志 / toast 默认值（页面层通常会覆盖 toast 指向 UI） ----------
  CG.log   = CG.log   || ((msg) => console.log('[CG] ' + msg));
  CG.toast = CG.toast || ((msg, type) => console.log('[CG][toast] ' + msg));

  // ====================================================================
  // ConnectionManager
  // ====================================================================
  const DC_LABEL = 'control';

  const TIMEOUT_WS  = 10000;
  const TIMEOUT_SDP = 20000;
  const TIMEOUT_ICE = 30000;

  const S_IDLE        = 'idle';
  const S_WS_CONN     = 'ws_connecting';
  const S_WAIT_ICE    = 'waiting_ice';
  const S_WS_OPEN     = 'ws_open';
  const S_OFFER_SENT  = 'offer_sent';
  const S_ANS_RCVD    = 'answer_received';
  const S_ICE_CONN    = 'ice_connecting';
  const S_CONNECTED   = 'connected';
  const S_ERROR       = 'error';

  const DEFAULT_ICE_SERVERS = [{ urls: ['stun:stun.l.google.com:19302'] }];

  class ConnectionManager {
    constructor(opts) {
      opts = opts || {};
      this._videoEl       = opts.videoEl || null;      // 注入视频元素（嵌入自有页面时）
      this._signalingBase = opts.signalingBase || '';  // 形如 'host:port' 或 ''（用当前域）
      this._iceServers    = opts.iceServers || null;   // 自定义 ICE（默认走 agent 下发）
      this._reconnect     = opts.reconnect || null;    // { max, backoff[] } 由 RemoteSession 使用

      this.ws = null;
      this.pc = null;
      this.dc = null;
      this.pcId = null;
      this._state = S_IDLE;
      this._timers = {};
      this._iceBuffer = [];
      this._remoteDescSet = false;
      this._iceServersCfg = DEFAULT_ICE_SERVERS.slice();
      this._iceServersReceived = false;
      this._iceServersResolve = null;
      this._debugTs = false; // 调试时间戳开关（debugcollect 启用时置 true）

      this._onWsMsg = null;  this._onWsClose = null;  this._onWsError = null;
      this._onIceCand = null; this._onIceState = null; this._onConnState = null;
      this._onSignaling = null; this._onTrack = null; this._onDcMsg = null;
    }

    get state() { return this._state; }
    _setState(s) {
      if (this._state === s) return;
      this._state = s;
      CG.log('[CM] state: ' + s);
      CG.events.emit('state-change', s);
    }

    // ---- timers ----
    _setTimer(name, fn, ms) {
      this._clearTimer(name);
      this._timers[name] = setTimeout(() => { delete this._timers[name]; fn(); }, ms);
    }
    _clearTimer(name) {
      if (this._timers[name]) { clearTimeout(this._timers[name]); delete this._timers[name]; }
    }
    _clearAllTimers() {
      Object.keys(this._timers).forEach(k => this._clearTimer(k));
    }

    // ================================================================
    //  connect — sequential, each step guards the next
    // ================================================================
    async connect(svcId, instId) {
      // 非空闲态进入（例如重连时旧连接仍残留 connected 态）：先静默拆除
      if (this._state !== S_IDLE && this._state !== S_ERROR) this.disconnect(true);
      if (!svcId || !instId) {
        CG.events.emit('error', { message: '请输入服务 ID 和设备 ID' });
        return;
      }
      this.disconnect(true);

      this._setState(S_WS_CONN);
      CG.events.emit('connecting', { svcId, instId });
      this._remoteDescSet = false;
      this._iceBuffer = [];

      // 1) WS
      try { await this._connectWS(svcId, instId); }
      catch (e) { this._fail('WebSocket 连接失败: ' + (e.message || '无法连接服务器')); return; }

      // 2) wait for ICE servers
      this._setState(S_WAIT_ICE);
      CG.events.emit('step', { id: 'ice-cfg', state: 'active' });
      try { await this._waitForICEServers(); }
      catch (_) { CG.log('[CM] ICE servers timeout, using defaults'); }
      this._emitIceCfgDetail();

      // 3) create PC + offer
      this._setState(S_WS_OPEN);
      try { this._createPC(); await this._createOffer(); }
      catch (e) { this._fail('WebRTC 初始化失败: ' + (e.message || '无法创建连接')); return; }
    }

    // ---- WS ----
    _connectWS(svcId, instId) {
      return new Promise((resolve, reject) => {
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        const base = this._signalingBase || location.host;
        const url = `${proto}//${base}/ws/browser/${encodeURIComponent(svcId)}/${encodeURIComponent(instId)}`;
        this.ws = new WebSocket(url);
        this._setTimer('ws_connect', () => { reject(new Error('超时')); }, TIMEOUT_WS);
        this.ws.onopen = () => {
          this._clearTimer('ws_connect');
          CG.events.emit('step', { id: 'ws', state: 'done' });
          this._setupWSHandlers();
          resolve();
        };
        this.ws.onerror = () => { this._clearTimer('ws_connect'); reject(new Error('连接失败')); };
      });
    }

    _setupWSHandlers() {
      if (!this.ws) return;
      this._onWsMsg = (e) => {
        try { this._handleWSMessage(JSON.parse(e.data)); } catch (_) {}
      };
      this._onWsClose = () => {
        if (this._state === S_ERROR) return;
        const wasConnected = this._state === S_CONNECTED;
        this._cleanupPC(); this._cleanupWS();
        this._setState(S_IDLE);
        // 已建立连接后掉线 → 交给上层自动重连；未建立时视为普通断开
        if (wasConnected) CG.events.emit('lost', { reason: 'ws-close' });
        else CG.events.emit('idle');
      };
      this._onWsError = () => { if (this._state !== S_CONNECTED) this._fail('WebSocket 通讯错误'); };
      this.ws.addEventListener('message', this._onWsMsg);
      this.ws.addEventListener('close', this._onWsClose);
      this.ws.addEventListener('error', this._onWsError);
    }

    _handleWSMessage(data) {
      switch (data.type) {
      case 'ice_servers':
        if (data.ice_servers && data.ice_servers.length > 0) {
          this._iceServersCfg = data.ice_servers;
          this._iceServersReceived = true;
          if (this._iceServersResolve) { this._iceServersResolve(); this._iceServersResolve = null; }
        }
        break;
      case 'answer':           this._handleAnswer(data); break;
      case 'ice_candidate':    this._handleIceCandidate(data); break;
      case 'error':            this._handleError(data); break;
      case 'stream_dead':
        // scrcpy 服务端进程中段死亡：与 ICE failed 同等处理，
        // 交给上层自动重连（预热池会对死亡会话冷启动新 server）
        CG.log('[CM] stream_dead: device stream died, reconnecting');
        if (this._state === S_CONNECTED) {
          this._cleanupPC(); this._cleanupWS();
          this._setState(S_IDLE);
          CG.events.emit('lost', { reason: 'stream-dead' });
        } else {
          this._fail('云端设备视频流中断');
        }
        break;
      case 'preempted':
        CG.log('[CM] preempted: device taken by another user');
        CG.events.emit('preempted');
        this.disconnect(true);
        break;
      }
    }

    _handleAnswer(data) {
      this._clearTimer('sdp');
      this.pcId = data.pc_id;
      this._setState(S_ANS_RCVD);
      CG.events.emit('step', { id: 'sdp', state: 'done' });
      this.pc.setRemoteDescription(new RTCSessionDescription(data.answer)).then(() => {
        this._remoteDescSet = true;
        if (this._iceBuffer.length) {
          this._iceBuffer.forEach(c => this.pc.addIceCandidate(new RTCIceCandidate(c)).catch(() => {}));
          this._iceBuffer = [];
        }
        this._setState(S_ICE_CONN);
        CG.events.emit('step', { id: 'ice', state: 'active' });
        this._setTimer('ice', () => {
          CG.events.emit('step', { id: 'ice', state: 'error' });
          this._fail('ICE 连接超时 (30s) — 可能原因: NAT 不支持直连 / 未配置 TURN 中转');
        }, TIMEOUT_ICE);
      }).catch(e => { this._fail('SDP 协商失败: ' + e.message); });
    }

    _handleIceCandidate(data) {
      if (!data.candidate || !this.pc) return;
      const cand = new RTCIceCandidate(data.candidate);
      if (this._remoteDescSet) { this.pc.addIceCandidate(cand).catch(() => {}); }
      else { this._iceBuffer.push(data.candidate); }
    }

    _handleError(data) {
      if (this._state !== S_CONNECTED) this._fail('信令协商失败: ' + (data.message || 'Unknown error'));
    }

    // ---- wait for ICE servers ----
    _waitForICEServers() {
      if (this._iceServersReceived) return Promise.resolve();
      return Promise.race([
        new Promise(r => { this._iceServersResolve = r; }),
        new Promise((_, rej) => setTimeout(() => rej(new Error('timeout')), 3000)),
      ]);
    }

    _emitIceCfgDetail() {
      const detail = (this._iceServersCfg || []).map(s => {
        const tag = (s.urls && s.urls[0]) || '';
        let label = 'STUN';
        if (tag.startsWith('turns:')) label = 'TURNS';
        else if (tag.startsWith('turn:')) label = 'TURN';
        return { label, urls: s.urls || [] };
      });
      CG.events.emit('step', { id: 'ice-cfg', state: 'done', detail });
    }

    // ================================================================
    //  PC creation — event handlers are the core state machine
    // ================================================================
    _createPC() {
      const iceServers = this._iceServers || this._iceServersCfg;
      this.pc = new RTCPeerConnection({ iceServers });
      this.dc = this.pc.createDataChannel(DC_LABEL, { ordered: true });

      // 设备 → 浏览器：剪贴板主动上报 / 调试日志协议（调试消息仅派发事件，
      // 由可选的 debugcollect 组件消费；开源构建无该组件时事件无人监听）
      this._onDcMsg = (e) => {
        const handle = (text) => {
          try {
            const m = JSON.parse(text);
            if (m.type === 'clipboard' && typeof m.text === 'string') {
              CG.events.emit('device-clipboard', { text: m.text });
            } else if (m.type === 'debug_log') {
              CG.events.emit('debug-log', { line: m.data && m.data.line });
            } else if (m.type === 'debug_log_batch') {
              CG.events.emit('debug-chunk', {
                seq: m.data && m.data.seq,
                total: m.data && m.data.total,
                chunk: m.data && m.data.chunk,
              });
            }
          } catch (_) {}
        };
        if (typeof e.data === 'string') {
          handle(e.data);
        } else if (e.data instanceof Blob) {
          e.data.text().then(handle).catch(() => {});
        } else if (e.data instanceof ArrayBuffer) {
          handle(new TextDecoder().decode(e.data));
        } else if (e.data && e.data.byteLength !== undefined) {
          handle(new TextDecoder().decode(e.data));
        }
      };
      this.dc.addEventListener('message', this._onDcMsg);

      this._onIceCand = (e) => {
        if (e.candidate && this.ws && this.ws.readyState === WebSocket.OPEN) {
          this.ws.send(JSON.stringify({
            type: 'ice_candidate',
            candidate: {
              candidate: e.candidate.candidate,
              sdpMid: e.candidate.sdpMid,
              sdpMLineIndex: e.candidate.sdpMLineIndex,
            },
          }));
        }
      };
      this.pc.addEventListener('icecandidate', this._onIceCand);

      // ---------- ICE state ----------
      this._onIceState = () => {
        const s = this.pc.iceConnectionState;
        CG.log('[CM] iceConnectionState: ' + s);
        if (s === 'connected' || s === 'completed') {
          this._clearTimer('ice');
          CG.events.emit('step', { id: 'ice', state: 'done' });
          CG.events.emit('link-degraded', { degraded: false });
        } else if (s === 'disconnected') {
          // 可恢复的抖动：提示弱网，不中断
          if (this._state === S_CONNECTED) CG.events.emit('link-degraded', { degraded: true });
        } else if (s === 'failed') {
          this._clearTimer('ice');
          if (this._state === S_CONNECTED) {
            // 会话中失败 → 自动重连流程
            this._cleanupPC(); this._cleanupWS();
            this._setState(S_IDLE);
            CG.events.emit('lost', { reason: 'ice-failed' });
          } else {
            CG.events.emit('step', { id: 'ice', state: 'error' });
            this._fail('ICE 连接失败 — 可能原因: NAT 不支持直连 / 未配置 TURN 中转');
          }
        }
      };
      this.pc.addEventListener('iceconnectionstatechange', this._onIceState);

      // ---------- connectionState: backup ----------
      this._onConnState = () => {
        const s = this.pc.connectionState;
        CG.log('[CM] connectionState: ' + s);
        if (s === 'failed') {
          if (this._state === S_CONNECTED) {
            this._cleanupPC(); this._cleanupWS();
            this._setState(S_IDLE);
            CG.events.emit('lost', { reason: 'pc-failed' });
          } else {
            this._fail('WebRTC 连接失败');
          }
        }
      };
      this.pc.addEventListener('connectionstatechange', this._onConnState);

      this._onSignaling = () => CG.log('[CM] signalingState: ' + this.pc.signalingState);
      this.pc.addEventListener('signalingstatechange', this._onSignaling);

      // ---------- Track ----------
      this._onTrack = (e) => {
        const stream = e.streams && e.streams[0];
        if (!stream) return;
        const video = this._videoEl || document.getElementById('remoteVideo');
        // 只在视频轨上绑定显示；srcObject 必须随重连替换——自动重连路径
        // ('lost') 不清空 srcObject，若沿用"已存在则跳过"会把新流挡在
        // 门外：解码在跑、画面却永远是旧流最后一帧，状态机卡死。
        if (e.track.kind === 'video' && video && video.srcObject !== stream) {
          video.srcObject = stream;

          // play() 兜底链：非静音 autoplay 需要 user activation（约 5s 有效
          // 期，慢连接会过期）；失败则退回静音播放（muted autoplay 恒允许），
          // 声音由 ui.js 的 unlock 监听器在用户下次手势时恢复。
          const tryPlay = async () => {
            try { await video.play(); return true; }
            catch (_) { return false; }
          };
          tryPlay().then(ok => {
            if (!ok && !video.muted) {
              CG.log('[CM] play() rejected (autoplay policy), retrying muted');
              video.muted = true;
              tryPlay();
            }
          });

          // track 到达即启动遥测：若等 'playing'（首帧解码）才启动，弱网下
          // 初始关键帧反复受损 → 永不出帧 → decoder_status / 停滞看门狗 /
          // FEC / 码率阶梯全部永不武装（零帧启动死锁）。
          CG.events.emit('track-video', e.track);

          video.addEventListener('playing', () => {
            if (this._state === S_ERROR) return;
            CG.events.emit('step', { id: 'video', state: 'done' });
            CG.events.emit('step', { id: 'first', state: 'done' });
            this._clearTimer('ice');
            this._setState(S_CONNECTED);
            this.sendWS({ type: 'first_frame', data: { ts: Date.now() } });
          }, { once: true });
        }

        e.track.addEventListener('ended', () => CG.log(`[CM] ${e.track.kind} ended`));
      };
      this.pc.addEventListener('track', this._onTrack);

      const vt = this.pc.addTransceiver('video', { direction: 'recvonly' });
      this.pc.addTransceiver('audio', { direction: 'recvonly' });
      // 播放延迟 50ms：给 jitter buffer 留出等待 NACK 重传的窗口，
      // 用少量延迟换取丢包场景下的可解码率（单位：秒）
      try { if (vt.receiver) vt.receiver.playoutDelayHint = 0.05; } catch (_) {}
    }

    // ---- Offer ----
    async _createOffer() {
      const offer = await this.pc.createOffer();
      await this.pc.setLocalDescription(offer);
      this._setState(S_OFFER_SENT);
      CG.events.emit('step', { id: 'sdp', state: 'active' });
      this.sendWS({ type: 'offer', offer: { sdp: offer.sdp, type: offer.type } });
      this._setTimer('sdp', () => this._fail('SDP 协商超时'), TIMEOUT_SDP);
    }

    // ================================================================
    //  fail / disconnect
    // ================================================================
    _fail(msg) {
      CG.log('[CM] FAIL: ' + msg);
      this._clearAllTimers();
      this._cleanupPC();
      this._cleanupWS();
      this._setState(S_ERROR);
      CG.events.emit('error', { message: msg, fatal: true });
    }

    disconnect(silent) {
      this._clearAllTimers();
      this._iceBuffer = [];
      this._remoteDescSet = false;
      this._iceServersReceived = false;
      this._iceServersResolve = null;
      this._cleanupPC();
      this._cleanupWS();
      // 静默拆除也必须复位状态机，否则 connect() 的前置检查会静默 return，
      // 重连流程表现为"界面在重连、实际什么也没发"（不发出 idle 事件，
      // 避免打断 reconnecting 界面）。
      if (silent) { this._state = S_IDLE; }
      else { this._setState(S_IDLE); CG.events.emit('idle'); }
    }

    _cleanupPC() {
      if (this.pc) {
        this.pc.removeEventListener('icecandidate', this._onIceCand);
        this.pc.removeEventListener('iceconnectionstatechange', this._onIceState);
        this.pc.removeEventListener('connectionstatechange', this._onConnState);
        this.pc.removeEventListener('signalingstatechange', this._onSignaling);
        this.pc.removeEventListener('track', this._onTrack);
        this.pc.close(); this.pc = null;
      }
      if (this.dc) {
        this.dc.removeEventListener('message', this._onDcMsg);
      }
      this.dc = null;
    }

    _cleanupWS() {
      if (this.ws) {
        this.ws.removeEventListener('message', this._onWsMsg);
        this.ws.removeEventListener('close', this._onWsClose);
        this.ws.removeEventListener('error', this._onWsError);
        this.ws.close(); this.ws = null;
      }
    }

    // ---- public helpers ----
    sendControl(type, data) { if (this.dc && this.dc.readyState === 'open') this.dc.send(JSON.stringify({ type, data })); }
    sendWS(msg) { if (this.ws && this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(msg)); }
    requestKeyframe() { this.sendControl('reset_video', {}); }
    sendKeyEvent(action, kc) { this.sendControl('inject_keycode', { action, keycode: kc, repeat: 0, metastate: 0 }); }
    /** 文字输入（可打印字符 / IME 上屏文本） */
    sendText(text) { if (text) this.sendControl('inject_text', { text }); }
    /** 设置设备剪贴板；paste=true 时设备端同时触发一次粘贴 */
    setClipboard(text, paste) {
      this.sendControl('set_clipboard', { sequence: Date.now() % 0x7fffffff, paste: !!paste, text });
    }
    /** 请求读取设备剪贴板（设备经 DataChannel 回 'clipboard' 消息） */
    getClipboard() { this.sendControl('get_clipboard', {}); }
    sendTouch(action, pid, x, y, w, h, p, btns) {
      const d = { action, pointer_id: pid, x, y, width: w, height: h, pressure: p, action_button: btns, buttons: btns };
      // 可选调试：debugcollect 启用时给触摸附带时间戳，供 agent 记录注入延迟
      if (this._debugTs) d.ts = Date.now();
      this.sendControl('inject_touch', d);
    }
    sendScroll(x, y, w, h, hs, vs) {
      this.sendControl('inject_scroll', { x, y, width: w, height: h, hscroll: hs, vscroll: vs, buttons: 0 });
    }
    sendNotification() { this.sendControl('inject_notification', {}); }
    /** 重置调试时间戳（调试收集停止时调用；开源构建中为 no-op） */
    debugReset() { this._debugTs = false; }
    /** 开启调试时间戳（由 debugcollect 调用；开源构建中不存在该组件） */
    debugEnableTs() { this._debugTs = true; }

    // ---- 调试协议（DataChannel，agent 闭源 hooks 消费） ----
    debugStart(durationMs) { this.sendControl('debug_start', { durationMs }); }
    debugStop()            { this.sendControl('debug_stop', {}); }
    debugFetch()           { this.sendControl('debug_fetch', {}); }

    get isConnected() { return this._state === S_CONNECTED; }
    get remoteStream() { return this._remoteStream; }
  }

  CG.ConnectionManager = ConnectionManager;

  // ====================================================================
  // StatsMonitor — 轮询 getStats，发事件 + 按需写入现有统计 id
  // ====================================================================
  class StatsMonitor {
    constructor() {
      this._interval = null;
      this._lastVideoBytes = 0;
      this._lastAudioBytes = 0;
      this._lastTime = 0;
      this._counter = 0;
      this._videoFrameCount = 0; // set externally by app
      this._hudVisible = false;
      this._resetStall();
    }

    _resetStall() {
      this._stall = { lastDecoded: -1, lastBytes: -1, secs: 0, fired: false };
    }

    get videoFrameCount() { return this._videoFrameCount; }
    set videoFrameCount(v) { this._videoFrameCount = v; }

    start(connMgr) {
      this.stop();
      this._conn = connMgr;
      this._lastVideoBytes = 0;
      this._lastAudioBytes = 0;
      this._lastTime = performance.now();
      this._counter = 0;
      this._connTypeReported = false;
      this._resetStall();
      this._interval = setInterval(() => this._tick(), 1000);
    }

    stop() {
      if (this._interval) { clearInterval(this._interval); this._interval = null; }
    }

    setHudVisible(v) { this._hudVisible = v; }

    async _tick() {
      const conn = this._conn;
      if (!conn || !conn.pc) return;

      let stats;
      try { stats = await conn.pc.getStats(); } catch (_) { return; }

      let videoRtp = null, audioRtp = null, candPair = null;
      stats.forEach(r => {
        if (r.type === 'inbound-rtp' && r.kind === 'video') videoRtp = r;
        if (r.type === 'inbound-rtp' && r.kind === 'audio') audioRtp = r;
        if (r.type === 'candidate-pair' && r.state === 'succeeded') candPair = r;
      });

      // 连接类型（首次判定）
      if (candPair && !this._connTypeReported) {
        const localCand  = candPair.localCandidateId  ? stats.get(candPair.localCandidateId)  : null;
        const remoteCand = candPair.remoteCandidateId ? stats.get(candPair.remoteCandidateId) : null;
        const localType  = localCand  ? localCand.candidateType  : '';
        const remoteType = remoteCand ? remoteCand.candidateType : '';
        const isRelay    = localType === 'relay' || remoteType === 'relay';
        this._connTypeReported = true;
        CG.events.emit('connection-type', {
          type: isRelay ? 'relay' : 'p2p',
          localType, remoteType,
          label: isRelay ? '中转 (TURN)' : '直连 (P2P)',
        });
      }

      const now = performance.now();
      const dt  = (now - this._lastTime) / 1000;
      let vBitrate = 0, vLossPct = '0.0', rttStr = '-', fpsStr = '-', rttMs = 0, fps = 0;

      // ---- Video ----
      if (videoRtp) {
        vBitrate = dt > 0 ? Math.round((videoRtp.bytesReceived - this._lastVideoBytes) * 8 / dt / 1000) : 0;
        this._lastVideoBytes = videoRtp.bytesReceived;

        const pktsRcv  = videoRtp.packetsReceived || 0;
        const pktsLost = videoRtp.packetsLost     || 0;
        const total    = pktsRcv + pktsLost;
        vLossPct = total > 0 ? ((pktsLost / total) * 100).toFixed(1) : '0.0';
        const nack   = videoRtp.nackCount   || 0;
        const pli    = videoRtp.pliCount    || 0;
        const dropped = videoRtp.framesDropped || 0;
        fps = videoRtp.framesPerSecond || 0;
        fpsStr = fps + ' fps';

        // 开发者面板（仅当页面存在对应 id 时写入）
        this._setText('statResolution', `${videoRtp.frameWidth || 0}×${videoRtp.frameHeight || 0}`);
        this._setText('statFps',        fpsStr);
        this._setText('statVBitrate',   `${vBitrate} kbps`);
        this._setText('statVJitter',    `${(videoRtp.jitter * 1000).toFixed(1)} ms`);
        this._setText('statVLoss',      `${pktsLost} (${vLossPct}%)`);
        this._setText('statVDropped',   `${dropped}`);
        this._setText('statVNackPli',   `${nack} / ${pli}`);

        // RTT
        if (candPair && candPair.currentRoundTripTime) {
          rttMs = candPair.currentRoundTripTime * 1000;
          rttStr = `${rttMs.toFixed(0)}ms`;
          this._setText('statRtt', `${rttMs.toFixed(1)} ms`);
        }

        // HUD
        this._updateHud(vLossPct, rttStr, fpsStr, `${vBitrate} kbps`);

        // 数值化指标 → UI 弱网判定
        CG.events.emit('stats', {
          loss: parseFloat(vLossPct) || 0,
          rtt: rttMs,
          fps,
          bitrate: vBitrate,
        });

        // 分辨率变化 → 坐标映射 + 旋转
        if (videoRtp.frameWidth && videoRtp.frameHeight) {
          CG.events.emit('resolution', { width: videoRtp.frameWidth, height: videoRtp.frameHeight });
        }

        // 媒体停滞（判「卡死」不判「静止」）
        this._checkStall(videoRtp);
      }

      // ---- Audio ----
      if (audioRtp) {
        const aBitrate = dt > 0 ? Math.round((audioRtp.bytesReceived - this._lastAudioBytes) * 8 / dt / 1000) : 0;
        this._lastAudioBytes = audioRtp.bytesReceived;
        const aPktsRcv  = audioRtp.packetsReceived || 0;
        const aPktsLost = audioRtp.packetsLost     || 0;
        const aTotal    = aPktsRcv + aPktsLost;
        const aLossPct  = aTotal > 0 ? ((aPktsLost / aTotal) * 100).toFixed(1) : '0.0';

        this._setText('statABitrate', `${aBitrate} kbps`);
        this._setText('statAJitter',  `${(audioRtp.jitter * 1000).toFixed(1)} ms`);
        this._setText('statALoss',    `${aLossPct}%`);
        this._setText('statADecoded', `${audioRtp.totalSamplesReceived || 0}`);
      }

      this._lastTime = now;
      this._counter++;

      // ---- 上报服务端 ----
      if (!conn.ws || conn.ws.readyState !== WebSocket.OPEN) return;

      if (this._counter % 3 === 0) {
        const v = videoRtp, a = audioRtp;
        conn.sendWS({
          type: 'decoder_status',
          data: {
            decoded:   v ? (v.framesDecoded || 0)     : 0,
            dropped:   v ? (v.framesDropped || 0)     : 0,
            pli:       v ? (v.pliCount    || 0)       : 0,
            fir:       v ? (v.firCount    || 0)       : 0,
            keyFrames: v ? (v.keyFramesReceived || 0) : 0,
            rendered:  this._videoFrameCount,
            size:      v ? `${v.frameWidth || 0}x${v.frameHeight || 0}` : '0x0',
            fps:       v ? (v.framesPerSecond || 0)   : 0,
            audioLost: a ? (a.packetsLost || 0)       : 0,
            rttMs:     candPair ? Math.round(candPair.currentRoundTripTime * 1000) : -1,
            lossRate:  parseFloat(vLossPct) || 0,
          },
        });
      }
      if (this._counter % 2 === 0 && videoRtp) {
        conn.sendWS({
          type: 'decoder_stats',
          data: { framesDecoded: videoRtp.framesDecoded || 0 },
        });
      }
    }

    // 媒体停滞：判「卡死」不判「静止」——码流仍在但 15s 不出帧 → 触发重连
    _checkStall(v) {
      if (!v || document.visibilityState === 'hidden') return;
      const st = this._stall;
      const decoded = v.framesDecoded || 0;
      const bytes   = v.bytesReceived || 0;

      if (st.lastDecoded < 0) { st.lastDecoded = decoded; st.lastBytes = bytes; return; }

      const decodedDelta = decoded - st.lastDecoded;
      const bytesDelta   = bytes   - st.lastBytes;
      st.lastDecoded = decoded;
      st.lastBytes   = bytes;

      // 正常出帧：复位
      if (decodedDelta > 0) { st.secs = 0; st.fired = false; return; }

      // 静止画面 / 编码器空闲：码流停止（<500B/s），不计时
      if (bytesDelta <= 500) { st.secs = 0; return; }

      // 卡死：包在到、帧不出
      st.secs++;
      if (st.secs >= 15 && !st.fired) {
        st.fired = true;
        CG.log('[STALL] 媒体停滞 15s（码流仍在） → 触发重连');
        CG.events.emit('media-stalled');
      }
    }

    _setText(id, val) {
      const el = document.getElementById(id);
      if (el) el.textContent = val;
    }

    _updateHud(lossPct, rttStr, fpsStr, brStr) {
      if (!this._hudVisible) return;

      const loss = parseFloat(lossPct) || 0;
      const rttMs = parseFloat(rttStr) || 0;
      const fps   = parseFloat(fpsStr) || 0;

      const lossCls = loss < 1 ? 'good' : loss < 5 ? 'warn' : 'bad';
      const rttCls  = rttMs < 80 ? 'good' : rttMs < 200 ? 'warn' : 'bad';
      const fpsCls  = fps >= 25 ? 'good' : fps >= 15 ? 'warn' : fps > 0 ? 'bad' : '';

      const hLoss = document.getElementById('hudLoss');
      const hRtt  = document.getElementById('hudRtt');
      const hFps  = document.getElementById('hudFps');
      const hBr   = document.getElementById('hudBitrate');

      if (hLoss) { hLoss.textContent = lossPct + '%';  hLoss.className = 'hud-val ' + lossCls; }
      if (hRtt)  { hRtt.textContent  = rttStr;          hRtt.className  = 'hud-val ' + rttCls;  }
      if (hFps)  { hFps.textContent  = fpsStr;          hFps.className  = 'hud-val ' + fpsCls;  }
      if (hBr)   { hBr.textContent   = brStr;           hBr.className   = 'hud-val good';       }
    }
  }

  CG.StatsMonitor = StatsMonitor;

  // ====================================================================
  // InputController — 输入 + 视口→设备坐标映射（元素可注入）
  // ====================================================================
  const POINTER_GENERIC = -2;
  const ACTION_DOWN  = 0;
  const ACTION_UP    = 1;
  const ACTION_MOVE  = 2;

  class InputController {
    constructor() {
      this._activePointers = new Map();  // browser pointerId → scrcpy pointerId
      this._resolution = { width: 1080, height: 1920 };
      this._conn = null;
      this._bound = false;
      this._composing = false;   // IME 组合中
      this._els = {};   // { overlay, video, container, root }
    }

    get resolution() { return this._resolution; }
    set resolution(r) { this._resolution = r; }

    /** 绑定到 DOM 与连接管理器；元素可注入，否则回退到既有 id */
    bind(connMgr, opts) {
      if (this._bound) return;
      this._conn = connMgr;
      this._bound = true;

      this._els.overlay   = (opts && opts.overlayEl)   || document.getElementById('controlOverlay');
      this._els.video     = (opts && opts.videoEl)     || document.getElementById('remoteVideo');
      this._els.container = (opts && opts.containerEl) || document.getElementById('videoContainer');
      this._els.root      = (opts && opts.rootEl)      || document.getElementById('root');

      const overlay = this._els.overlay;
      if (!overlay) return;

      // ---- Pointer events ----
      overlay.addEventListener('pointerdown', e => this._onPointerDown(e));
      overlay.addEventListener('pointermove', e => this._onPointerMove(e));
      overlay.addEventListener('pointerup',   e => this._onPointerUp(e));
      overlay.addEventListener('pointercancel', e => this._onPointerCancel(e));
      overlay.addEventListener('wheel',       e => this._onWheel(e), { passive: false });

      // ---- Keyboard ----
      window.addEventListener('keydown', e => this._onKeyDown(e));
      window.addEventListener('keyup',   e => this._onKeyUp(e));

      // ---- IME composition ----
      window.addEventListener('compositionstart', () => { this._composing = true; });
      window.addEventListener('compositionend',   e => this._onCompositionEnd(e));

      // ---- Clipboard: browser → device ----
      window.addEventListener('paste', e => this._onPaste(e));

      // ---- Resize / orientation ----
      window.addEventListener('resize', () => this._updateOverlay());
      window.matchMedia('(orientation: portrait)').addEventListener('change', () => {
        setTimeout(() => this._updateOverlay(), 200);
        setTimeout(() => this._updateOverlay(), 500);
      });

      // ---- Video dimension tracking ----
      const video = this._els.video;
      if (video) {
        video.addEventListener('loadedmetadata', () => {
          if (video.videoWidth > 0 && video.videoHeight > 0) {
            this._resolution.width  = video.videoWidth;
            this._resolution.height = video.videoHeight;
          }
          this._updateOverlay();
        });
        video.addEventListener('resize', () => {
          if (video.videoWidth > 0 && video.videoHeight > 0) {
            this._resolution.width  = video.videoWidth;
            this._resolution.height = video.videoHeight;
          }
          this._updateOverlay();
        });

        new ResizeObserver(() => this._updateOverlay()).observe(video);
        const container = this._els.container;
        if (container) new ResizeObserver(() => this._updateOverlay()).observe(container);
      }

      // Initial overlay layout
      setTimeout(() => this._updateOverlay(), 500);
    }

    /** 是否处于本地 UI 输入态（连接表单 / 键盘面板等），此时不拦截键盘与剪贴板 */
    _isLocalEditing(e) {
      const t = e.target;
      return !!(t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable));
    }

    _ready() { return !!(this._conn && this._conn.isConnected); }

    /** Compute the visible video frame rectangle within the container */
    _getFrameRect() {
      const video = this._els.video;
      const vw = video ? video.videoWidth  : 0;
      const vh = video ? video.videoHeight : 0;
      if (vw === 0 || vh === 0) {
        const r = this._resolution;
        return { left: 0, top: 0, width: 0, height: 0, scaleX: 1, scaleY: 1 };
      }

      const container = this._els.container;
      const cW = container ? container.offsetWidth : 0;
      const cH = container ? container.offsetHeight : 0;
      const vRatio = vw / vh;
      const cRatio = cW / cH;

      let fW, fH, offX, offY;
      if (cRatio > vRatio) {
        fH = cH; fW = cH * vRatio;
        offX = (cW - fW) / 2; offY = 0;
      } else {
        fW = cW; fH = cW / vRatio;
        offX = 0; offY = (cH - fH) / 2;
      }
      return {
        left: offX, top: offY,
        width: fW, height: fH,
        scaleX: vw / fW, scaleY: vh / fH,
      };
    }

    /** Position the controlOverlay over the visible video frame */
    _updateOverlay() {
      const overlay = this._els.overlay;
      if (!overlay) return;
      const f = this._getFrameRect();
      overlay.style.left   = f.left   + 'px';
      overlay.style.top    = f.top    + 'px';
      overlay.style.width  = f.width  + 'px';
      overlay.style.height = f.height + 'px';
    }

    /** Convert viewport clientX/Y → device coordinate */
    _clientToDevice(clientX, clientY) {
      const overlay = this._els.overlay;
      const ovr = overlay.getBoundingClientRect();
      const root = this._els.root;
      const rotated = root && root.classList.contains('rotated');
      const f = this._getFrameRect();

      let localX, localY;
      if (rotated) {
        // After rotate(90deg) clockwise on #root:
        //   visual X+  →  CSS Y-
        //   visual Y+  →  CSS X+
        const rx = ovr.width  > 0 ? (clientX - ovr.left) / ovr.width  : 0;
        const ry = ovr.height > 0 ? (clientY - ovr.top)  / ovr.height : 0;
        localX = f.left + ry * f.width;
        localY = f.top  + (1 - rx) * f.height;
      } else {
        localX = ovr.width  > 0 ? (clientX - ovr.left) / ovr.width  * overlay.offsetWidth  : 0;
        localY = ovr.height > 0 ? (clientY - ovr.top)  / ovr.height * overlay.offsetHeight : 0;
      }

      const baseX = rotated ? (localX - f.left) : localX;
      const baseY = rotated ? (localY - f.top)  : localY;
      return {
        x: Math.max(0, Math.min(Math.round(baseX * f.scaleX), this._resolution.width)),
        y: Math.max(0, Math.min(Math.round(baseY * f.scaleY), this._resolution.height)),
      };
    }

    // ---- Pointer handlers ----
    _onPointerDown(e) {
      e.preventDefault();
      const overlay = this._els.overlay;
      overlay.setPointerCapture(e.pointerId);
      const pid = e.pointerType === 'touch' ? (e.pointerId + 1) : POINTER_GENERIC;
      this._activePointers.set(e.pointerId, pid);
      if (this._conn) {
        const pos = this._clientToDevice(e.clientX, e.clientY);
        this._conn.sendTouch(ACTION_DOWN, pid, pos.x, pos.y,
          this._resolution.width, this._resolution.height,
          e.pressure || 1.0, 0x01);
      }
    }

    _onPointerMove(e) {
      if (!this._activePointers.has(e.pointerId) || !this._conn) return;
      e.preventDefault();
      const pid = this._activePointers.get(e.pointerId);
      const pos = this._clientToDevice(e.clientX, e.clientY);
      this._conn.sendTouch(ACTION_MOVE, pid, pos.x, pos.y,
        this._resolution.width, this._resolution.height,
        e.pressure || 1.0, 0x01);
    }

    _onPointerUp(e) {
      if (!this._activePointers.has(e.pointerId) || !this._conn) return;
      e.preventDefault();
      const pid = this._activePointers.get(e.pointerId);
      const pos = this._clientToDevice(e.clientX, e.clientY);
      this._conn.sendTouch(ACTION_UP, pid, pos.x, pos.y,
        this._resolution.width, this._resolution.height, 0, 0);
      this._activePointers.delete(e.pointerId);
    }

    _onPointerCancel(e) { this._onPointerUp(e); }

    // ---- Wheel handler ----
    _onWheel(e) {
      e.preventDefault();
      if (!this._conn) return;
      const pos = this._clientToDevice(e.clientX, e.clientY);
      this._conn.sendScroll(pos.x, pos.y,
        this._resolution.width, this._resolution.height,
        e.deltaX / 50, e.deltaY / 50);
    }

    // ================================================================
    //  Keyboard：功能键走 inject_keycode，可打印字符走 inject_text
    // ================================================================
    _onKeyDown(e) {
      if (!this._ready() || this._isLocalEditing(e)) return;
      if (e.isComposing || this._composing) return;   // IME 组合期间交给 compositionend

      const kc = this._keyFromEvent(e);
      if (kc !== null) {
        if (e.repeat) return;
        e.preventDefault();
        this._conn.sendKeyEvent(0, kc);
        return;
      }

      // 可打印字符（含空格）→ inject_text；带 Ctrl/Meta/Alt 的组合键不拦截
      if (!e.repeat && e.key && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        e.preventDefault();
        this._conn.sendText(e.key);
      }
    }

    _onKeyUp(e) {
      if (!this._ready() || this._isLocalEditing(e)) return;
      const kc = this._keyFromEvent(e);
      if (kc !== null) {
        e.preventDefault();
        this._conn.sendKeyEvent(1, kc);
        return;
      }
      // 字符键没有对应的 keyup 语义（inject_text 是瞬时事件），仅阻止默认行为
      if (e.key && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        e.preventDefault();
      }
    }

    /** IME 上屏：整段文本一次注入（中文、日文等组合字符的唯一可靠通道） */
    _onCompositionEnd(e) {
      this._composing = false;
      if (!this._ready() || this._isLocalEditing(e)) return;
      if (e.data) this._conn.sendText(e.data);
    }

    /** 粘贴：浏览器剪贴板 → 设备剪贴板并立即粘贴到当前输入框 */
    _onPaste(e) {
      if (!this._ready() || this._isLocalEditing(e)) return;
      const text = e.clipboardData && e.clipboardData.getData('text');
      if (!text) return;
      e.preventDefault();
      this._conn.setClipboard(text, true);
      CG.events.emit('clipboard-sent', { length: text.length });
    }

    _keyFromEvent(e) {
      const map = {
        Backspace: 67, Home: 3, ContextMenu: 187,
        AudioVolumeUp: 24, AudioVolumeDown: 25,
        Power: 26, ArrowUp: 19, ArrowDown: 20,
        ArrowLeft: 21, ArrowRight: 22,
        Enter: 66, Escape: 4,
        Tab: 61, Delete: 67,
      };
      return map[e.code] != null ? map[e.code] : null;
    }

    /** Called externally when resolution changes from stats */
    onResolutionChange(w, h) {
      this._resolution.width  = w;
      this._resolution.height = h;
      this._updateOverlay();
    }
  }

  CG.InputController = InputController;

  // ====================================================================
  // relayQuery — 信令中继查询（占用检测 / 抢占）
  // ====================================================================
  CG.relayQuery = function(svcId, instId, payload, opts) {
    return new Promise((resolve, reject) => {
      const host = (opts && opts.host) || CG._signalingBase || location.host;
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${host}/ws/relay/${encodeURIComponent(svcId)}/${encodeURIComponent(instId)}`;
      const ws = new WebSocket(url);
      const relayId = (crypto.randomUUID ? crypto.randomUUID() : 'xxxx-xxxx-xxxx'.replace(/x/g, () => ((Math.random() * 16) | 0).toString(16)));
      const timer = setTimeout(() => { try { ws.close(); } catch (e) {} reject(new Error('超时')); }, 5000);
      let resolved = false;

      ws.onopen = () => {
        ws.send(JSON.stringify({ type: 'relay', relay_id: relayId, payload: payload || {} }));
      };

      ws.onmessage = (e) => {
        try {
          const msg = JSON.parse(e.data);
          if (msg.type === 'relay' && msg.relay_id === relayId && msg.payload) {
            resolved = true;
            clearTimeout(timer);
            try { ws.close(); } catch (e) {}
            resolve(msg);
          }
        } catch (_) {}
      };

      ws.onerror = () => { clearTimeout(timer); reject(new Error('连接失败')); };
      ws.onclose = () => {
        if (!resolved) { clearTimeout(timer); reject(new Error('连接关闭')); }
      };
    });
  };

  // ====================================================================
  // RemoteSession — 一体化门面（dev 深度定制 / 嵌入自有页面用）
  //
  // 封装 ConnectionManager + StatsMonitor + InputController，统一重连策略，
  // 对外只暴露 on/off + connect/disconnect + 控制透传，便于在任意页面嵌入。
  // 普通整页（app.js 编排）无需使用本门面，直接调用 CG.ConnectionManager 即可。
  // ====================================================================
  class RemoteSession {
    constructor(opts) {
      opts = opts || {};
      this._opts = opts;
      this._cm = new CG.ConnectionManager(opts);
      this._stats = new CG.StatsMonitor();
      this._input = new CG.InputController();
      this._statsEnabled = opts.stats !== false;
      this._inputOpts = opts.input || null;
    }

    on(evt, fn)  { CG.events.on(evt, fn); return this; }
    off(evt, fn) { CG.events.off(evt, fn); return this; }
    once(evt, fn) {
      const w = (d) => { fn(d); this.off(evt, w); };
      return this.on(evt, w);
    }

    connect(svc, inst) {
      const p = this._cm.connect(svc, inst);
      // 视频轨道到达后：启动统计 + 绑定输入
      const onTrack = () => {
        if (this._statsEnabled) this._stats.start(this._cm);
        this._input.bind(this._cm, this._inputOpts);
      };
      CG.events.on('track-video', onTrack);
      return p;
    }

    disconnect(silent) { this._cm.disconnect(silent); this._stats.stop(); }

    // ---- 控制透传 ----
    sendText(t)            { this._cm.sendText(t); }
    sendKeyEvent(a, kc)    { this._cm.sendKeyEvent(a, kc); }
    sendTouch(a, pid, x, y, w, h, p, b) { this._cm.sendTouch(a, pid, x, y, w, h, p, b); }
    sendScroll(x, y, w, h, dx, dy)      { this._cm.sendScroll(x, y, w, h, dx, dy); }
    setClipboard(t, paste) { this._cm.setClipboard(t, paste); }
    getClipboard()         { this._cm.getClipboard(); }
    sendControl(act, p)    { this._cm.sendControl(act, p); }
    requestKeyframe()      { this._cm.requestKeyframe(); }
    setQuality(level)      { this._cm.sendControl('set_quality', { level }); }

    get isConnected() { return this._cm.isConnected; }
    get state()       { return this._cm.state; }
    get input()       { return this._input; }
    get stats()       { return this._stats; }
    get connection()  { return this._cm; }
  }

  CG.RemoteSession = RemoteSession;

  CG.SDK_VERSION = '1.0.0';
})();
