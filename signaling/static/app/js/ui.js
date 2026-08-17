/**
 * UIModule — 用户版界面模块
 *
 * 全部界面由 #root[data-ui] 状态驱动：
 *   idle / connecting / connected / reconnecting / failed
 *
 * 对外 API（由 app.js 调用）：
 *   setUI(state)                    切换界面状态
 *   showIdle({svcId, instId, lastSession})
 *   showProgress()                  进入连接页并重置进度
 *   setStageInfo(idx, title, sub)   更新阶段文案
 *   setProgress(pct)                环形进度 0-100
 *   markStage(idx, done)            阶段圆点状态
 *   showError({title,msg,detail,primaryText})
 *   updateReconnect(attempt, max, countdownText)
 *   showPreemptConfirm(svc, inst) → Promise<boolean>
 *   showExitConfirm()             → Promise<boolean>
 *   toast(msg, type)
 *   applyAutoRotate()
 *   setNetBadge(show) / updateNetCard(rtt, loss)
 *   audioMuted (getter)
 */
(function() {
  'use strict';

  const CG = window.CG;

  const RING_LEN = 326.7;

  class UIModule {
    constructor() {
      this._audioMuted = true;        // 首次手势解锁前保持静音（自动播放策略）
      this._soundOn = true;           // 用户意图：声音开
      this._orientMode = 'follow';    // follow | portrait | landscape
      this._initFab();
      this._initSheets();
      this._initSettings();
      this._initAudio();
      this._initFullscreen();
      this._initAutoRotate();
      this._initUnlockAudio();
      this._initNetBadge();
    }

    // ================================================================
    // 状态切换
    // ================================================================
    setUI(state) {
      document.getElementById('root').dataset.ui = state;
      const fab = document.getElementById('fab');
      if (fab) fab.classList.toggle('hidden', state !== 'connected');
      document.getElementById('root').classList.toggle('frozen', state === 'reconnecting');
      if (state !== 'connected') { this.closeFabMenu(); this.closeSheets(); }
      if (state === 'connected') this._resetFabIdle();
    }

    // ================================================================
    // 入口页
    // ================================================================
    showIdle(opts = {}) {
      this.setUI('idle');
      const { svcId, instId, lastSession } = opts;
      const title = document.getElementById('idleTitle');
      const sub = document.getElementById('idleSub');
      const ls = document.getElementById('lastSession');

      if (svcId && instId) {
        if (title) title.textContent = '设备就绪';
        if (sub) sub.textContent = svcId + ' / ' + instId;
        const setVal = (id, v) => { const el = document.getElementById(id); if (el) el.value = v; };
        setVal('svcInput', svcId); setVal('instInput', instId);
      } else {
        if (title) title.textContent = '云游戏';
        if (sub) sub.textContent = '免下载 · 即点即玩';
      }

      if (lastSession && svcId && instId) {
        document.getElementById('lsName').textContent = instId;
        ls.classList.remove('hidden');
      } else {
        ls.classList.add('hidden');
      }
    }

    // ================================================================
    // 连接进度页
    // ================================================================
    showProgress() {
      this.setUI('connecting');
      this.setProgress(0);
      this.setStageInfo(0, '正在连接云端设备…', '预计需要几秒钟');
      document.querySelectorAll('#connSteps li').forEach(li => li.className = '');
      this.markStage(0, 'doing');
    }

    setStageInfo(idx, title, sub, slow) {
      this._setText('connStageTitle', title);
      const subEl = document.getElementById('connStageSub');
      if (subEl) { subEl.textContent = sub || ''; subEl.classList.toggle('slow', !!slow); }
      document.querySelectorAll('#connSteps li').forEach((li, i) => {
        li.classList.toggle('done', i < idx);
        li.classList.toggle('doing', i === idx);
      });
    }

    markStage(idx, state) {
      const li = document.querySelector(`#connSteps li[data-i="${idx}"]`);
      if (li) li.className = state || '';
    }

    setProgress(pct) {
      this._pct = pct;
      this._setText('connPct', Math.round(pct) + '%');
      const ring = document.getElementById('connRingFill');
      if (ring) ring.style.strokeDashoffset = RING_LEN * (1 - pct / 100);
    }

    getProgress() { return this._pct || 0; }

    // ================================================================
    // 错误页
    // ================================================================
    showError({ title, msg, detail, primaryText }) {
      this.setUI('failed');
      this._setText('errTitle', title || '出错了');
      this._setText('errMsg', msg || '');
      this._setText('errDetail', detail || '');
      this._setText('errorRetryBtn', primaryText || '重试连接');
      document.getElementById('errDetail')?.classList.remove('open');
      this._setText('errDetailToggle', '查看技术详情');
    }

    // ================================================================
    // 重连蒙层
    // ================================================================
    updateReconnect(attempt, max, countdownText) {
      this._setText('rcSub', `第 ${attempt}/${max} 次尝试`);
      this._setText('rcCount', countdownText || '');
    }

    // ================================================================
    // 对话框（Promise 风格）
    // ================================================================
    showPreemptConfirm(svcId, instId) {
      return new Promise((resolve) => {
        const dialog = document.getElementById('preemptDialog');
        if (!dialog) { resolve(true); return; }
        this._setText('preemptInfo', `设备 ${instId} 正在被其他用户使用，你可以等对方结束，或直接接管使用。`);
        dialog.classList.remove('hidden');

        const done = (ok) => {
          dialog.classList.add('hidden');
          document.getElementById('preemptConfirmBtn')?.removeEventListener('click', onOk);
          document.getElementById('preemptCancelBtn')?.removeEventListener('click', onNo);
          resolve(ok);
        };
        const onOk = () => done(true);
        const onNo = () => done(false);
        document.getElementById('preemptConfirmBtn')?.addEventListener('click', onOk);
        document.getElementById('preemptCancelBtn')?.addEventListener('click', onNo);
      });
    }

    showExitConfirm() {
      return new Promise((resolve) => {
        const dialog = document.getElementById('dlgExit');
        dialog.classList.remove('hidden');
        const done = (ok) => {
          dialog.classList.add('hidden');
          document.getElementById('exitOkBtn')?.removeEventListener('click', onOk);
          document.getElementById('exitCancelBtn')?.removeEventListener('click', onNo);
          resolve(ok);
        };
        const onOk = () => done(true);
        const onNo = () => done(false);
        document.getElementById('exitOkBtn')?.addEventListener('click', onOk);
        document.getElementById('exitCancelBtn')?.addEventListener('click', onNo);
      });
    }

    // ================================================================
    // Toast
    // ================================================================
    toast(msg, type) {
      const el = document.getElementById('toast');
      if (!el) return;
      el.textContent = msg;
      el.className = 'show' + (type ? ' ' + type : '');
      clearTimeout(this._toastTimer);
      this._toastTimer = setTimeout(() => el.className = '', 2200);
    }

    // ================================================================
    // 弱网徽标
    // ================================================================
    _initNetBadge() {
      document.getElementById('netBadge')?.addEventListener('click', () => {
        document.getElementById('netCard')?.classList.toggle('hidden');
      });
    }

    setNetBadge(show) {
      document.getElementById('netBadge')?.classList.toggle('hidden', !show);
      if (!show) document.getElementById('netCard')?.classList.add('hidden');
    }

    updateNetCard(rtt, loss) {
      this._setText('ncRtt', rtt != null ? Math.round(rtt) + ' ms' : '-');
      this._setText('ncLoss', loss != null ? loss.toFixed(1) + ' %' : '-');
    }

    // ================================================================
    // 悬浮球：可拖动 / 闲置半透明 / 点击展开菜单（旋转坐标系感知）
    // ================================================================
    _initFab() {
      const fab = document.getElementById('fab');
      const menu = document.getElementById('fabMenu');
      if (!fab || !menu) return;

      let drag = false, moved = false;
      let sx = 0, sy = 0, fx = 0, fy = 0;

      const getContainerSize = () => {
        const root = document.getElementById('root');
        return root.classList.contains('rotated')
          ? { w: root.offsetWidth, h: root.offsetHeight }
          : { w: innerWidth, h: innerHeight };
      };

      const clamp = () => {
        const fw = fab.offsetWidth || 52, fh = fab.offsetHeight || 52;
        const { w: cw, h: ch } = getContainerSize();
        fx = Math.max(8, Math.min(fx, cw - fw - 8));
        fy = Math.max(8, Math.min(fy, ch - fh - 8));
        fab.style.left = fx + 'px';
        fab.style.top = fy + 'px';
        fab.style.right = 'auto';
      };

      const snapEdge = (animate) => {
        const fw = fab.offsetWidth || 52;
        const { w: cw } = getContainerSize();
        fx = (fx + fw / 2) < cw / 2 ? 8 : cw - fw - 8;
        if (animate) fab.style.transition = 'left .25s ease-out, top .25s ease-out, opacity .5s ease';
        clamp();
        if (animate) setTimeout(() => { fab.style.transition = 'opacity .5s ease'; }, 280);
      };

      { // 初始位置：右侧中部偏下
        const { w: cw, h: ch } = getContainerSize();
        fx = cw - 64; fy = Math.round(ch * 0.42);
        clamp();
      }

      fab.addEventListener('pointerdown', e => {
        if (e.pointerType === 'mouse' && e.button !== 0) return;
        e.preventDefault();
        fab.setPointerCapture(e.pointerId);
        fab.style.transition = 'opacity .5s ease';
        drag = true; moved = false;
        sx = e.clientX; sy = e.clientY;
        fx = parseInt(fab.style.left) || 0;
        fy = parseInt(fab.style.top) || 0;
        this._resetFabIdle();
      });

      fab.addEventListener('pointermove', e => {
        if (!drag) return;
        const dx = e.clientX - sx, dy = e.clientY - sy;
        if (Math.abs(dx) > 4 || Math.abs(dy) > 4) moved = true;
        const root = document.getElementById('root');
        if (root && root.classList.contains('rotated')) {
          fx += dy; fy -= dx;
        } else {
          fx += dx; fy += dy;
        }
        sx = e.clientX; sy = e.clientY;
        clamp();
        this._resetFabIdle();
      });

      fab.addEventListener('pointerup', e => {
        if (!drag) return;
        fab.releasePointerCapture(e.pointerId);
        drag = false;
        if (moved) snapEdge(true);
        else this.toggleFabMenu();
        this._resetFabIdle();
      });

      window.addEventListener('resize', () => snapEdge(true));
      CG.events.on('root-rotation-changed', () => snapEdge(true));
    }

    _resetFabIdle() {
      const fab = document.getElementById('fab');
      if (!fab) return;
      fab.classList.remove('idle');
      clearTimeout(this._fabIdleTimer);
      this._fabIdleTimer = setTimeout(() => {
        if (document.getElementById('fabMenu')?.classList.contains('hidden')) fab.classList.add('idle');
      }, 3000);
    }

    toggleFabMenu() {
      const fab = document.getElementById('fab');
      const menu = document.getElementById('fabMenu');
      if (!menu.classList.contains('hidden')) { this.closeFabMenu(); return; }

      const root = document.getElementById('root');
      const rotated = root.classList.contains('rotated');
      const W = rotated ? root.offsetWidth : innerWidth;
      const H = rotated ? root.offsetHeight : innerHeight;
      const mw = 170, mh = menu.scrollHeight || 330;
      let mx = (parseInt(fab.style.left) || 0) + 26 - mw / 2;
      mx = Math.max(8, Math.min(mx, W - mw - 8));
      let my = (parseInt(fab.style.top) || 0) - mh - 12;
      if (my < 8) my = (parseInt(fab.style.top) || 0) + 52 + 12;
      menu.style.left = mx + 'px';
      menu.style.top = my + 'px';
      menu.classList.remove('hidden');
    }

    closeFabMenu() {
      document.getElementById('fabMenu')?.classList.add('hidden');
    }

    // ================================================================
    // 底部弹层
    // ================================================================
    _initSheets() {
      document.getElementById('sheetMask')?.addEventListener('click', () => this.closeSheets());
    }

    openSheet(id) {
      this.closeFabMenu();
      document.getElementById('sheetMask')?.classList.remove('hidden');
      document.getElementById(id)?.classList.add('open');
    }

    closeSheets() {
      document.getElementById('sheetMask')?.classList.add('hidden');
      document.querySelectorAll('.sheet').forEach(s => s.classList.remove('open'));
      document.getElementById('clipConfirm')?.classList.add('hidden');
    }

    // ================================================================
    // 设置
    // ================================================================
    _initSettings() {
      // 画面方向三档
      document.querySelectorAll('#orientRow .pill').forEach(p => {
        p.addEventListener('click', () => {
          document.querySelectorAll('#orientRow .pill').forEach(x => x.classList.remove('active'));
          p.classList.add('active');
          this._orientMode = p.dataset.o;
          this._applyRotate();
          CG.toast(this._orientMode === 'follow' ? '画面将跟随内容旋转' : `已${p.textContent}`);
        });
      });

      // 声音
      document.getElementById('tglAudio')?.addEventListener('change', (e) => {
        this._soundOn = e.target.checked;
        this._audioMuted = !this._soundOn;
        const video = document.getElementById('remoteVideo');
        if (video) video.muted = this._audioMuted;
        CG.toast(this._soundOn ? '声音已开启' : '已静音');
      });

      // HUD
      document.getElementById('hudToggle')?.addEventListener('change', (e) => {
        const vis = e.target.checked;
        document.getElementById('netHud')?.classList.toggle('hidden', !vis);
        CG.events.emit('hud-toggle', vis);
      });

      // 错误页：技术详情折叠
      document.getElementById('errDetailToggle')?.addEventListener('click', () => {
        const d = document.getElementById('errDetail');
        d.classList.toggle('open');
        this._setText('errDetailToggle', d.classList.contains('open') ? '收起技术详情' : '查看技术详情');
      });

      // 入口页：手动输入展开
      document.getElementById('manualToggle')?.addEventListener('click', () => {
        document.getElementById('manualForm')?.classList.toggle('open');
      });
    }

    // ================================================================
    // 音频
    // ================================================================
    _initAudio() { /* 由 _initSettings 中的开关与 _initUnlockAudio 共同管理 */ }

    get audioMuted() { return this._audioMuted; }

    _initUnlockAudio() {
      let unlocked = false;
      const unlock = () => {
        if (unlocked) return;
        unlocked = true;
        this._audioMuted = !this._soundOn;
        const video = document.getElementById('remoteVideo');
        if (video) video.muted = this._audioMuted;
      };
      document.body.addEventListener('touchstart', unlock, { once: true });
      document.body.addEventListener('click', unlock, { once: true });
    }

    // ================================================================
    // 全屏
    // ================================================================
    _initFullscreen() {
      const btn = document.getElementById('fullscreenToggle');
      if (!btn) return;
      const updateLabel = () => {
        btn.textContent = document.fullscreenElement ? '退出全屏' : '全屏模式';
      };
      btn.addEventListener('click', () => {
        if (document.fullscreenElement) document.exitFullscreen();
        else {
          this._applyRotate();
          document.documentElement.requestFullscreen().catch(() => {});
        }
      });
      document.addEventListener('fullscreenchange', () => {
        updateLabel();
        setTimeout(() => this._applyRotate(), 100);
      });
      updateLabel();
    }

    // ================================================================
    // 旋转 FSM：follow / portrait / landscape 三档
    // ================================================================
    _initAutoRotate() {
      window.addEventListener('resize', () => this._applyRotate());
      window.matchMedia('(orientation: portrait)').addEventListener('change', () => {
        setTimeout(() => this._applyRotate(), 200);
      });
    }

    applyAutoRotate() { this._applyRotate(); }

    _applyRotate() {
      const root  = document.getElementById('root');
      const video = document.getElementById('remoteVideo');
      if (!root || !video) return;

      if (this._orientMode === 'portrait') {
        root.classList.remove('rotated');
      } else if (this._orientMode === 'landscape') {
        root.classList.add('rotated');
      } else {
        const vw = video.videoWidth, vh = video.videoHeight;
        if (vw === 0 || vh === 0) {
          root.classList.remove('rotated');
        } else {
          const videoLandscape = vw > vh;
          const viewportLandscape = innerWidth >= innerHeight;
          root.classList.toggle('rotated', videoLandscape !== viewportLandscape);
        }
      }
      CG.events.emit('root-rotation-changed');
    }

    // ================================================================
    // Helpers
    // ================================================================
    _setText(id, val) {
      const el = document.getElementById(id);
      if (el) el.textContent = val;
    }
  }

  CG.UIModule = UIModule;
})();
