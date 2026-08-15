/**
 * StatsMonitor — polls WebRTC getStats() every 1 second.
 *
 * 用户版改动：
 *   - 每秒发出 'stats' 事件（数值化 rtt/loss/fps/bitrate），供 UI 做弱网徽标判定
 *   - HUD / 开发者面板的元素 id 与原调试版保持一致，无需改动写入逻辑
 *   - 媒体停滞检测：码流还在到（bytesReceived 增长）但持续不出帧
 *     （framesDecoded 零增长）满 15s，发 'media-stalled' 事件由 app 直接
 *     全量重连。静止画面码流本身停止，不计时不误触发。无任何中间恢复
 *     动作（关键帧请求/解码器复位已验证无效，不做）。
 *
 * Reports to server:
 *   - decoder_status every 3 seconds (detailed health)
 *   - decoder_stats  every 2 seconds (framesDecoded)
 */
(function() {
  'use strict';

  const CG = window.CG;

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
      try {
        stats = await conn.pc.getStats();
      } catch (_) { return; }

      let videoRtp = null, audioRtp = null, candPair = null;
      stats.forEach(r => {
        if (r.type === 'inbound-rtp' && r.kind === 'video') videoRtp = r;
        if (r.type === 'inbound-rtp' && r.kind === 'audio') audioRtp = r;
        if (r.type === 'candidate-pair' && r.state === 'succeeded') candPair = r;
      });

      // Detect connection type from candidate pair (first time only)
      if (candPair && !this._connTypeReported) {
        const localCand  = candPair.localCandidateId  ? stats.get(candPair.localCandidateId)  : null;
        const remoteCand = candPair.remoteCandidateId ? stats.get(candPair.remoteCandidateId) : null;
        const localType  = localCand  ? localCand.candidateType  : '';
        const remoteType = remoteCand ? remoteCand.candidateType : '';
        const isRelay    = localType === 'relay' || remoteType === 'relay';
        this._connTypeReported = true;
        CG.events.emit('connection-type', {
          type:       isRelay ? 'relay' : 'p2p',
          localType:  localType,
          remoteType: remoteType,
          label:      isRelay ? '中转 (TURN)' : '直连 (P2P)',
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

        // 开发者面板
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

        // Notify app of resolution change
        if (videoRtp.frameWidth && videoRtp.frameHeight) {
          CG.events.emit('resolution', {
            width: videoRtp.frameWidth,
            height: videoRtp.frameHeight,
          });
        }

        // 媒体停滞检测（检测逻辑同原看门狗，但动作只有重连一级）
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

      // ---- Report to server ----
      if (!conn.ws || conn.ws.readyState !== WebSocket.OPEN) return;

      if (this._counter % 3 === 0) {
        const v = videoRtp, a = audioRtp;
        conn.sendWS({
          type: 'decoder_status',
          data: {
            decoded:   v ? (v.framesDecoded || 0)       : 0,
            dropped:   v ? (v.framesDropped || 0)       : 0,
            pli:       v ? (v.pliCount    || 0)         : 0,
            fir:       v ? (v.firCount    || 0)         : 0,
            keyFrames: v ? (v.keyFramesReceived || 0)   : 0,
            rendered:  this._videoFrameCount,
            size:      v ? `${v.frameWidth || 0}x${v.frameHeight || 0}` : '0x0',
            fps:       v ? (v.framesPerSecond || 0)     : 0,
            audioLost: a ? (a.packetsLost || 0)         : 0,
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

    // ================================================================
    // 媒体停滞检测：判"卡死"不判"静止"
    //
    // 静止画面：编码器不出帧 → bytesReceived 同样不增长 → 不计时。
    // 管线卡死：bytesReceived 仍在增长（发送端还在发，例如 GCC 地板码率）
    // 而 framesDecoded 零增长 → 连续 15s 判定卡死，发一次 'media-stalled'
    // 交给 app 全量重连（中间级恢复已验证无效，直接重连）。恢复出帧前不
    // 重复触发；页面不可见时不计时。
    // ================================================================
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
})();
