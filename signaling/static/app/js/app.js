/**
 * App — 云游戏用户版入口（编排层）
 *
 * 界面状态机（#root[data-ui]）：
 *   idle → connecting → connected ⇄ reconnecting → failed → idle
 *
 * 与真实管线的衔接：
 *   - ConnectionManager 的 6 个技术 step 映射为 3 段用户旅程进度
 *   - 'lost' 事件（会话中断线）进入自动重连：指数退避 ×5，耗尽落错误页
 *   - 原始错误消息翻译为友好文案，技术细节折叠展示
 *   - 剪贴板：paste 事件 / 手动发送 → set_clipboard；get_clipboard 与设备
 *     主动上报 → 写入本地 + Toast；键盘面板与 inject_text 打通
 */
(function() {
  'use strict';

  window.CG = window.CG || {};

  // ---- 事件总线 ----
  const listeners = {};
  window.CG.events = {
    on(evt, fn) { (listeners[evt] = listeners[evt] || []).push(fn); },
    off(evt, fn) {
      const arr = listeners[evt];
      if (arr) { const i = arr.indexOf(fn); if (i >= 0) arr.splice(i, 1); }
    },
    emit(evt, data) { (listeners[evt] || []).forEach(fn => fn(data)); },
  };

  window.CG.log = (msg) => console.log('[CG] ' + msg);
  window.CG.toast = (msg, type) => { if (app && app.ui) app.ui.toast(msg, type); };

  // ---- 技术步骤 → 用户旅程映射 ----
  const JOURNEY = {
    'ws':      { stage: 0, pct: 20 },
    'ice-cfg': { stage: 0, pct: 35, doneTitle: '正在建立网络通道…', doneSub: '正在为你选择最优线路' },
    'sdp':     { stage: 1, pct: 55 },
    'ice':     { stage: 1, pct: 75, doneTitle: '即将进入，请稍候…', doneSub: '正在加载游戏画面' },
    'video':   { stage: 2, pct: 90 },
    'first':   { stage: 2, pct: 100 },
  };

  const RC_MAX = 5;
  const RC_BACKOFF = [1.5, 2, 3, 4, 5];  // 秒，指数退避

  const app = {
    conn: null, stats: null, input: null, actions: null, ui: null,

    _svc: null, _inst: null,
    _rc: { active: false, attempt: 0, timer: null, tick: null },
    _pctAnim: null,
    _weakTicks: 0, _goodTicks: 0,
    _lastSession: false,

    init() {
      this.conn    = new CG.ConnectionManager();
      this.stats   = new CG.StatsMonitor();
      this.input   = new CG.InputController();
      this.actions = new CG.QuickActions();
      this.ui      = new CG.UIModule();

      this._wireEvents();
      this._wireButtons();
      this.input.bind(this.conn);
      this.actions.bind(this.conn);

      // URL 参数直达
      const p = new URLSearchParams(location.search);
      const s = p.get('service_id') || p.get('s');
      const d = p.get('device_id')   || p.get('d');
      this.ui.showIdle({ svcId: s, instId: d });

      window._app = app;
    },

    // ================================================================
    // 事件接线
    // ================================================================
    _wireEvents() {
      const E = CG.events;

      // 技术步骤 → 旅程进度
      E.on('step', d => this._onStep(d));

      E.on('connecting', () => {
        if (!this._rc.active) this.ui.showProgress();
      });

      E.on('track-video', () => {
        const video = document.getElementById('remoteVideo');
        if (video) {
          video.muted = this.ui.audioMuted;
          video.play().catch(e => CG.log('video play blocked: ' + e.message));
        }
        this.stats.start(this.conn);
        this._startFrameCallback();
        setTimeout(() => this.ui.applyAutoRotate(), 500);
      });

      E.on('state-change', state => {
        if (state === 'connected') this._onConnected();
      });

      // 会话中断线 → 自动重连
      E.on('lost', info => {
        CG.log('[APP] connection lost: ' + (info && info.reason));
        this._startReconnect();
      });

      // 媒体停滞 15s（码流还在、帧不出）→ 与断线同等处理，全量重连
      E.on('media-stalled', () => {
        CG.log('[APP] media stalled 15s, forcing reconnect');
        if (this.conn.isConnected) this._startReconnect();
      });

      // 普通断开（用户主动 / 未连接时 WS 关闭）
      E.on('idle', () => {
        if (this._rc.active) return;  // 重连过程中的中间态不复位
        this._reset();
      });

      // 首连 / 重连尝试失败
      E.on('error', d => {
        if (this._rc.active) { this._scheduleReconnect(); return; }
        this.stats.stop();
        this._showFriendlyError(d.message);
      });

      // 被其他用户抢占
      E.on('preempted', () => {
        this._cancelReconnect();
        this.stats.stop();
        this._cleanupSession();
        this.ui.toast('设备已被其他用户接管，连接已断开', 'error');
        this.ui.showIdle({ svcId: this._svc, instId: this._inst, lastSession: this._lastSession });
      });

      // 连接模式（P2P / TURN）→ 开发者面板
      E.on('connection-type', d => this.ui.setDevConnType(d.label));

      // HUD 开关 → stats 写入许可
      E.on('hud-toggle', v => this.stats.setHudVisible(v));

      // 分辨率变化 → 坐标映射 + 旋转
      E.on('resolution', d => {
        this.input.onResolutionChange(d.width, d.height);
        this.ui.applyAutoRotate();
      });

      // 实时指标 → 弱网徽标判定
      E.on('stats', d => this._onStats(d));

      // ICE 抖动（可恢复） → 弱网提示
      E.on('link-degraded', d => {
        if (d.degraded) this._showWeakBadge();
      });

      // 设备 → 浏览器剪贴板（主动上报或 get_clipboard 响应）
      E.on('device-clipboard', d => {
        navigator.clipboard?.writeText(d.text).catch(() => {});
        this.ui.toast(`云端剪贴板已写入本地（${d.text.length} 个字符）`, 'success');
        this.ui.closeSheets();
      });

      // 浏览器 → 设备（paste 事件路径）
      E.on('clipboard-sent', d => {
        this.ui.toast(`已粘贴到云端（${d.length} 个字符）`, 'success');
      });
    },

    // ================================================================
    // 按钮接线
    // ================================================================
    _wireButtons() {
      const $ = id => document.getElementById(id);

      $('connectBtn')?.addEventListener('click', () => this._connect());
      $('reconnectLastBtn')?.addEventListener('click', () => this._connect());
      $('cancelConnBtn')?.addEventListener('click', () => this.conn.disconnect());
      $('errorRetryBtn')?.addEventListener('click', () => this._connect());
      $('errorHomeBtn')?.addEventListener('click', () => {
        this.ui.showIdle({ svcId: this._svc, instId: this._inst, lastSession: this._lastSession });
      });

      // 重连蒙层
      $('rcNowBtn')?.addEventListener('click', () => this._attemptReconnect());
      $('rcExitBtn')?.addEventListener('click', async () => {
        this._cancelReconnect();
        this.conn.disconnect();   // → 'idle' → _reset()
      });

      // 悬浮球菜单中非系统按键项
      document.querySelectorAll('#fabMenu button[data-act]').forEach(btn => {
        const act = btn.dataset.act;
        if (['back', 'home', 'app_switch'].includes(act)) return; // actions.js 处理
        btn.addEventListener('click', () => {
          if (act === 'clipboard') this.ui.openSheet('sheetClipboard');
          else if (act === 'keyboard') {
            this.ui.openSheet('sheetKeyboard');
            setTimeout(() => $('kbdInput')?.focus(), 350);
          }
          else if (act === 'settings') this.ui.openSheet('sheetSettings');
          else if (act === 'exit') this._requestExit();
        });
      });

      // 剪贴板面板
      $('clipSendBtn')?.addEventListener('click', async () => {
        let text = '';
        try { text = await navigator.clipboard.readText(); } catch (_) {}
        if (!text) { this.ui.toast('本地剪贴板为空或无读取权限', 'error'); return; }
        this.conn.setClipboard(text, false);
        this.ui.toast(`已发送 ${text.length} 个字符到设备`, 'success');
      });
      $('clipGetBtn')?.addEventListener('click', () => {
        $('clipConfirm')?.classList.remove('hidden');
      });
      $('clipCancelBtn')?.addEventListener('click', () => {
        $('clipConfirm')?.classList.add('hidden');
      });
      $('clipOkBtn')?.addEventListener('click', () => {
        $('clipConfirm')?.classList.add('hidden');
        this.conn.getClipboard();   // 响应经 device-clipboard 事件回来
        this.ui.toast('正在读取云端剪贴板…');
      });

      // 清晰度档位（清晰/高清/原画，只切码率，分辨率不变）
      document.querySelectorAll('#qualityRow .pill').forEach(btn => {
        btn.addEventListener('click', () => this._setQuality(btn.dataset.q));
      });
      this._restoreQuality();

      // 键盘面板
      const sendKbd = () => {
        const inp = $('kbdInput');
        const text = inp?.value || '';
        if (!text || !this.conn.isConnected) return;
        this.conn.sendText(text);
        if ($('kbdEnter')?.checked) {
          this.conn.sendKeyEvent(0, 66);
          setTimeout(() => this.conn.sendKeyEvent(1, 66), 50);
        }
        this.ui.toast(`已输入 ${text.length} 个字符`, 'success');
        inp.value = '';
      };
      $('kbdSendBtn')?.addEventListener('click', sendKbd);
      $('kbdInput')?.addEventListener('keydown', e => {
        if (e.key === 'Enter') { e.preventDefault(); sendKbd(); }
      });
    },

    // ================================================================
    // 连接流程（含占用检测与抢占）
    // ================================================================
    async _connect() {
      const svc = (document.getElementById('svcInput')?.value || '').trim()
               || (new URLSearchParams(location.search)).get('s') || '';
      const inst = (document.getElementById('instInput')?.value || '').trim()
                || (new URLSearchParams(location.search)).get('d') || '';
      if (!svc || !inst) {
        this.ui.toast('请输入服务 ID 和设备 ID', 'error');
        document.getElementById('manualForm')?.classList.add('open');
        return;
      }
      this._svc = svc; this._inst = inst;

      // URL 可书签化
      const url = new URL(location.href);
      url.searchParams.set('s', svc); url.searchParams.set('d', inst);
      if (url.href !== location.href) history.replaceState(null, '', url.href);

      this.ui.showProgress();
      this.ui.setStageInfo(0, '正在连接云端设备…', '检测设备状态…');

      try {
        const resp = await CG.relayQuery(svc, inst, { type: 'query_status' });
        if (resp.payload && resp.payload.busy) {
          const ok = await this.ui.showPreemptConfirm(svc, inst);
          if (!ok) {
            this.ui.showIdle({ svcId: svc, instId: inst, lastSession: this._lastSession });
            return;
          }
          this.ui.setStageInfo(0, '正在接管设备…', '请稍候');
          await CG.relayQuery(svc, inst, { type: 'preempt' });
        }
      } catch (e) {
        CG.log('[APP] status check failed, connecting directly: ' + e.message);
      }

      this.ui.setStageInfo(0, '正在连接云端设备…', '预计需要几秒钟');
      this.conn.connect(svc, inst);
    },

    // ================================================================
    // 进度旅程
    // ================================================================
    _onStep(d) {
      const j = JOURNEY[d.id];
      if (!j) return;
      if (d.state === 'done' || d.state === 'active') {
        const pct = d.state === 'done' ? j.pct : Math.max(j.pct - 12, 5);
        this._tweenProgress(pct);
        if (d.state === 'active') this.ui.markStage(j.stage, 'doing');
        if (d.state === 'done') {
          this.ui.markStage(j.stage, 'doing');
          if (j.doneTitle) this.ui.setStageInfo(j.stage + 1 > 2 ? 2 : j.stage + 1, j.doneTitle, j.doneSub);
        }
        // ICE 阶段耗时提醒（管理预期）
        if (d.id === 'ice' && d.state === 'active') {
          clearTimeout(this._slowTimer);
          this._slowTimer = setTimeout(() => {
            const root = document.getElementById('root');
            if (root.dataset.ui === 'connecting') {
              this.ui.setStageInfo(1, '正在建立网络通道…', '网络似乎有点慢，仍在尝试…', true);
            }
          }, 8000);
        }
        if (d.id === 'ice' && d.state === 'done') clearTimeout(this._slowTimer);
      }
    },

    _tweenProgress(target) {
      cancelAnimationFrame(this._pctAnim);
      const step = () => {
        const cur = this.ui.getProgress();
        const next = cur + (target - cur) * 0.12;
        if (Math.abs(target - next) < 0.5) { this.ui.setProgress(target); return; }
        this.ui.setProgress(next);
        this._pctAnim = requestAnimationFrame(step);
      };
      this._pctAnim = requestAnimationFrame(step);
    },

    // ================================================================
    // 已连接
    // ================================================================
    _onConnected() {
      const wasReconnect = this._rc.active;
      this._cancelReconnect();
      this._lastSession = true;
      this.ui.setUI('connected');
      this.ui.setNetBadge(false);
      this._weakTicks = 0; this._goodTicks = 0;
      this.ui.toast(wasReconnect ? '已重新连接' : '已进入游戏', 'success');
      // 重发清晰度档位：agent 侧虽会按档位持久化，但 agent 重启 /
      // 抢占接管后状态可能丢失，这里兜底同步一次
      this._applyQuality(false);
    },

    // ================================================================
    // 清晰度档位（固定分辨率，只切码率）
    // ================================================================
    _quality() {
      return localStorage.getItem('cg-quality') || 'original';
    },

    _setQuality(level) {
      localStorage.setItem('cg-quality', level);
      document.querySelectorAll('#qualityRow .pill').forEach(b =>
        b.classList.toggle('active', b.dataset.q === level));
      this._applyQuality(true);
    },

    _restoreQuality() {
      const q = this._quality();
      document.querySelectorAll('#qualityRow .pill').forEach(b =>
        b.classList.toggle('active', b.dataset.q === q));
    },

    _applyQuality(notify) {
      if (!this.conn.isConnected) return;
      const level = this._quality();
      this.conn.sendControl('set_quality', { level });
      if (notify) {
        const names = { clear: '清晰', hd: '高清', original: '原画' };
        this.ui.toast(`已切换到${names[level] || level}画质`, 'success');
      }
    },

    // ================================================================
    // 自动重连
    // ================================================================
    _startReconnect() {
      if (this._rc.active || !this._svc) return;
      this._rc.active = true;
      this._rc.attempt = 0;
      // 断链路径下连接可能仍残留 connected 态，先静默拆除，
      // 使 ConnectionManager.connect() 的状态前置检查能通过
      this.conn.disconnect(true);
      this.stats.stop();
      this.ui.setNetBadge(false);
      this.ui.setUI('reconnecting');
      this._scheduleReconnect();
    },

    _scheduleReconnect() {
      if (!this._rc.active) return;
      this._rc.attempt++;
      if (this._rc.attempt > RC_MAX) {
        this._cancelReconnect();
        this._showFriendlyError('__exhausted__');
        return;
      }
      const wait = RC_BACKOFF[Math.min(this._rc.attempt - 1, RC_BACKOFF.length - 1)];
      let left = wait;
      this.ui.updateReconnect(this._rc.attempt, RC_MAX, `${left}s 后自动重试`);
      clearInterval(this._rc.tick);
      this._rc.tick = setInterval(() => {
        left -= 1;
        if (left <= 0) { clearInterval(this._rc.tick); this._attemptReconnect(); }
        else this.ui.updateReconnect(this._rc.attempt, RC_MAX, `${left}s 后自动重试`);
      }, 1000);
    },

    _attemptReconnect() {
      if (!this._rc.active) return;
      clearInterval(this._rc.tick);
      this.ui.updateReconnect(Math.max(this._rc.attempt, 1), RC_MAX, '正在连接…');
      try {
        const p = this.conn.connect(this._svc, this._inst);
        // 连接失败会走 'error' 事件 → _scheduleReconnect()；这里兜底同步异常，
        // 防止重连循环静默死掉
        if (p && p.catch) p.catch(err => {
          CG.log('[APP] reconnect attempt threw: ' + (err && err.message));
          if (this._rc.active) this._scheduleReconnect();
        });
      } catch (err) {
        CG.log('[APP] reconnect attempt threw: ' + (err && err.message));
        if (this._rc.active) this._scheduleReconnect();
      }
    },

    _cancelReconnect() {
      this._rc.active = false;
      clearInterval(this._rc.tick);
      this._rc.tick = null;
    },

    // ================================================================
    // 用户主动断开
    // ================================================================
    async _requestExit() {
      const ok = await this.ui.showExitConfirm();
      if (ok) this.conn.disconnect();   // → 'idle' → _reset()
    },

    // ================================================================
    // 错误翻译
    // ================================================================
    _showFriendlyError(raw) {
      const detail = `${raw || 'unknown'}\nstate: ${this.conn.state}\ndevice: ${this._svc || '-'} / ${this._inst || '-'}\ntime: ${new Date().toLocaleString()}`;

      let e;
      if (raw === '__exhausted__') {
        e = { title: '重连失败', primaryText: '再次重试',
              msg: '网络持续不稳定，自动重连未成功。请检查 Wi-Fi 或切换网络后再试。' };
      } else if (/ICE|NAT|TURN/.test(raw)) {
        e = { title: '当前网络连不上这台设备', primaryText: '重试连接',
              msg: '你的网络环境（NAT / 防火墙）限制了直连。试试切换 Wi-Fi 或蜂窝网络？' };
      } else if (/SDP|信令/.test(raw)) {
        e = { title: '设备暂时没有响应', primaryText: '重试连接',
              msg: '云端设备忙或不在线，稍等片刻再试。' };
      } else {
        e = { title: '连不上云端设备', primaryText: '重试连接',
              msg: '服务暂时无法到达，请检查网络后重试。' };
      }
      this.ui.showError({ ...e, detail });
    },

    // ================================================================
    // 弱网徽标（连续 3 秒差 → 亮出；连续 5 秒好 → 熄灭）
    // ================================================================
    _onStats(d) {
      const bad = d.loss > 3 || d.rtt > 150;
      if (bad) { this._weakTicks++; this._goodTicks = 0; }
      else { this._goodTicks++; this._weakTicks = 0; }

      if (this._weakTicks >= 3) this._showWeakBadge();
      if (this._goodTicks >= 5) this.ui.setNetBadge(false);

      this.ui.updateNetCard(d.rtt, d.loss);
    },

    _showWeakBadge() {
        const root = document.getElementById('root');
        if (root.dataset.ui === 'connected') this.ui.setNetBadge(true);
    },

    // ================================================================
    // 收尾
    // ================================================================
    _cleanupSession() {
      this._stopFrameCallback();
      this.stats.stop();
      const video = document.getElementById('remoteVideo');
      if (video) video.srcObject = null;
      document.getElementById('netHud')?.classList.add('hidden');
      document.getElementById('hudToggle') && (document.getElementById('hudToggle').checked = false);
      this.stats.setHudVisible(false);
      this.ui.setNetBadge(false);
    },

    _reset() {
      this._cleanupSession();
      this.ui.showIdle({ svcId: this._svc, instId: this._inst, lastSession: this._lastSession });
    },

    // ---- 首帧计数（上报 decoder_status 用） ----
    _startFrameCallback() {
      if (this.rafId) return;
      const video = document.getElementById('remoteVideo');
      if (!video || !video.requestVideoFrameCallback) return;
      const schedule = () => {
        this.rafId = video.requestVideoFrameCallback(() => {
          this.stats.videoFrameCount = (this.stats.videoFrameCount || 0) + 1;
          schedule();
        });
      };
      schedule();
    },

    _stopFrameCallback() {
      const video = document.getElementById('remoteVideo');
      if (this.rafId && video && video.cancelVideoFrameCallback) {
        video.cancelVideoFrameCallback(this.rafId);
        this.rafId = null;
      }
    },
  };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => app.init());
  } else {
    app.init();
  }
})();
