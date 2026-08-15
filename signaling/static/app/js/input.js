/**
 * InputController — handles touch/pointer/keyboard/scroll input,
 * coordinate mapping from viewport → device coordinates.
 *
 * The controlOverlay sits on top of the visible video frame area.
 * Coordinates are mapped from CSS space → device resolution.
 *
 * 用户版改动（键盘与剪贴板补齐）：
 *   1. 键盘不再只映射 12 个功能键——可打印字符（字母/数字/符号/空格）
 *      统一走 inject_text，功能键仍走 inject_keycode
 *   2. 支持 IME：composition 期间不拦截按键，compositionend 时把整段
 *      上屏文本经 inject_text 发出（中文输入可用）
 *   3. 新增 paste 监听：浏览器内 Ctrl+V / 长按粘贴 → set_clipboard(paste:true)
 *   4. 本地 UI 输入框（连接表单、键盘输入面板）聚焦时不拦截按键与粘贴
 *   5. 仅在已连接状态下发送控制消息
 */
(function() {
  'use strict';

  const CG = window.CG;

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
    }

    get resolution() { return this._resolution; }
    set resolution(r) { this._resolution = r; }

    /** Bind to DOM and connection manager */
    bind(connMgr) {
      if (this._bound) return;
      this._conn = connMgr;
      this._bound = true;

      const overlay = document.getElementById('controlOverlay');
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
      const video = document.getElementById('remoteVideo');
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
        const container = document.getElementById('videoContainer');
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
      const video = document.getElementById('remoteVideo');
      const vw = video.videoWidth  || this._resolution.width;
      const vh = video.videoHeight || this._resolution.height;
      if (vw === 0 || vh === 0) return { left: 0, top: 0, width: 0, height: 0, scaleX: 1, scaleY: 1 };

      const container = document.getElementById('videoContainer');
      const cW = container.offsetWidth;
      const cH = container.offsetHeight;
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
      const overlay = document.getElementById('controlOverlay');
      if (!overlay) return;
      const f = this._getFrameRect();
      overlay.style.left   = f.left   + 'px';
      overlay.style.top    = f.top    + 'px';
      overlay.style.width  = f.width  + 'px';
      overlay.style.height = f.height + 'px';
    }

    /** Convert viewport clientX/Y → device coordinate */
    _clientToDevice(clientX, clientY) {
      const overlay = document.getElementById('controlOverlay');
      const ovr = overlay.getBoundingClientRect();
      const root = document.getElementById('root');
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
      const overlay = document.getElementById('controlOverlay');
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

    _onPointerCancel(e) {
      this._onPointerUp(e);
    }

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
      const text = e.clipboardData?.getData('text');
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
      return map[e.code] ?? null;
    }

    /** Called externally when resolution changes from stats */
    onResolutionChange(w, h) {
      this._resolution.width  = w;
      this._resolution.height = h;
      this._updateOverlay();
    }
  }

  CG.InputController = InputController;
})();
