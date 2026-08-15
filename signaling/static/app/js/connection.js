/**
 * ConnectionManager — manages WebSocket + WebRTC lifecycle.
 *
 * States (linear):
 *   idle → ws_connecting → waiting_ice → ws_open → offer_sent
 *                                                    ↓
 *                                           answer_received
 *                                                    ↓
 *                                            ice_connecting
 *                                               ↓         ↘
 *                                          connected     error
 *
 * "connected" = ontrack fires + video element playing.
 *
 * 用户版改动：
 *   - 连接建立后掉线（WS 关闭 / ICE failed）不再直接报错，而是发出 'lost' 事件，
 *     由上层进入自动重连流程
 *   - ICE disconnected（可恢复的抖动）发出 'link-degraded' 事件
 *   - 新增 inject_text / set_clipboard / get_clipboard 发送助手
 *   - 设备→浏览器剪贴板改为事件 'device-clipboard' 上报
 */
(function() {
  'use strict';

  const CG = window.CG = window.CG || {};

  const DEFAULT_ICE_SERVERS = [{ urls: 'stun:stun.l.google.com:19302' }];
  const DC_LABEL = 'control';

  const TIMEOUT_WS         = 10000;
  const TIMEOUT_SDP        = 20000;
  const TIMEOUT_ICE        = 30000;

  const S_IDLE        = 'idle';
  const S_WS_CONN     = 'ws_connecting';
  const S_WAIT_ICE    = 'waiting_ice';
  const S_WS_OPEN     = 'ws_open';
  const S_OFFER_SENT  = 'offer_sent';
  const S_ANS_RCVD    = 'answer_received';
  const S_ICE_CONN    = 'ice_connecting';
  const S_CONNECTED   = 'connected';
  const S_ERROR       = 'error';

  class ConnectionManager {
    constructor() {
      this.ws = null;
      this.pc = null;
      this.dc = null;
      this.pcId = null;
      this._state = S_IDLE;
      this._timers = {};
      this._iceBuffer = [];
      this._remoteDescSet = false;
      this._iceServers = DEFAULT_ICE_SERVERS.slice();

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
      // 再继续，而不是静默 return 让重连空转
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
        const url = `${proto}//${location.host}/ws/browser/${svcId}/${instId}`;
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
          this._iceServers = data.ice_servers;
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
      const detail = (this._iceServers || []).map(s => {
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
      this.pc = new RTCPeerConnection({ iceServers: this._iceServers });
      this.dc = this.pc.createDataChannel(DC_LABEL, { ordered: true });

      // 设备 → 浏览器：剪贴板主动上报
      this._onDcMsg = (e) => {
        try {
          const m = JSON.parse(e.data);
          if (m.type === 'clipboard' && typeof m.text === 'string') {
            CG.events.emit('device-clipboard', { text: m.text });
          }
        } catch (_) {}
      };
      this.dc.addEventListener('message', this._onDcMsg);

      this._onIceCand = (e) => {
        if (e.candidate && this.ws?.readyState === WebSocket.OPEN)
          this.ws.send(JSON.stringify({ type: 'ice_candidate', candidate: { candidate: e.candidate.candidate, sdpMid: e.candidate.sdpMid, sdpMLineIndex: e.candidate.sdpMLineIndex } }));
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
        const stream = e.streams?.[0];
        if (!stream) return;
        const video = document.getElementById('remoteVideo');
        // 只在视频轨上绑定显示；srcObject 必须随重连替换——自动重连路径
        // ('lost') 不清空 srcObject，若沿用"已存在则跳过"会把新流挡在
        // 门外：解码在跑、画面却永远是旧流最后一帧，状态机卡死。
        if (e.track.kind === 'video' && video.srcObject !== stream) {
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
      this.ws.send(JSON.stringify({ type: 'offer', offer: { sdp: offer.sdp, type: offer.type } }));
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
    sendControl(type, data) { if (this.dc?.readyState === 'open') this.dc.send(JSON.stringify({type,data})); }
    sendWS(msg) { if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(msg)); }
    requestKeyframe() { this.sendControl('reset_video', {}); }
    sendKeyEvent(action, kc) { this.sendControl('inject_keycode', {action, keycode:kc, repeat:0, metastate:0}); }
    /** 文字输入（可打印字符 / IME 上屏文本） */
    sendText(text) { if (text) this.sendControl('inject_text', { text }); }
    /** 设置设备剪贴板；paste=true 时设备端同时触发一次粘贴 */
    setClipboard(text, paste) {
      this.sendControl('set_clipboard', { sequence: Date.now() % 0x7fffffff, paste: !!paste, text });
    }
    /** 请求读取设备剪贴板（设备经 DataChannel 回 'clipboard' 消息） */
    getClipboard() { this.sendControl('get_clipboard', {}); }
    sendTouch(action, pid, x, y, w, h, p, btns) { this.sendControl('inject_touch', {action, pointer_id:pid, x, y, width:w, height:h, pressure:p, action_button:btns, buttons:btns}); }
    sendScroll(x, y, w, h, hs, vs) { this.sendControl('inject_scroll', {x, y, width:w, height:h, hscroll:hs, vscroll:vs, buttons:0}); }
    sendNotification() { this.sendControl('inject_notification', {}); }
    get isConnected() { return this._state === S_CONNECTED; }
  }

  CG.ConnectionManager = ConnectionManager;

  // ---- Relay helper: query device status or preempt without triggering bound/unbound ----
  CG.relayQuery = function(svcId, instId, payload) {
    return new Promise((resolve, reject) => {
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${location.host}/ws/relay/${svcId}/${instId}`;
      const ws = new WebSocket(url);
      const relayId = crypto.randomUUID ? crypto.randomUUID() :
        'xxxx-xxxx-xxxx'.replace(/x/g, () => ((Math.random()*16)|0).toString(16));
      const timer = setTimeout(() => { ws.close(); reject(new Error('超时')); }, 5000);
      let resolved = false;

      ws.onopen = () => {
        ws.send(JSON.stringify({ type: 'relay', relay_id: relayId, payload }));
      };

      ws.onmessage = (e) => {
        try {
          const msg = JSON.parse(e.data);
          if (msg.type === 'relay' && msg.relay_id === relayId && msg.payload) {
            resolved = true;
            clearTimeout(timer);
            ws.close();
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
})();
