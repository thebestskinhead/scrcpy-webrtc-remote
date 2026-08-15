/**
 * QuickActions — 系统按键与视频修复动作。
 * 用户版：绑定悬浮球菜单 [data-act] 按钮与设置页的"修复视频流"按钮。
 */
(function() {
  'use strict';

  const CG = window.CG;

  const KEY_MAP = {
    home:       { kc: 3,   name: '主页' },
    back:       { kc: 4,   name: '返回' },
    app_switch: { kc: 187, name: '最近任务' },
    volume_up:   { kc: 24, name: '音量+' },
    volume_down: { kc: 25, name: '音量-' },
    power:       { kc: 26, name: '电源键' },
  };

  class QuickActions {
    constructor() {
      this._conn = null;
      this._bound = false;
    }

    bind(connMgr) {
      if (this._bound) return;
      this._conn = connMgr;
      this._bound = true;

      // 悬浮球菜单中的系统按键
      document.querySelectorAll('#fabMenu button[data-act]').forEach(btn => {
        const act = btn.dataset.act;
        if (!KEY_MAP[act]) return;   // 其余 act（clipboard/settings/exit…）由 app.js 处理
        btn.addEventListener('click', () => this._handle(act));
      });

      // 设置页：修复视频流
      document.getElementById('keyframeBtn')?.addEventListener('click', () => {
        this._conn.requestKeyframe();
        CG.toast('已请求刷新画面', 'success');
      });
    }

    _handle(action) {
      const def = KEY_MAP[action];
      if (!def || !this._conn.isConnected) return;
      this._conn.sendKeyEvent(0, def.kc);
      setTimeout(() => this._conn.sendKeyEvent(1, def.kc), 50);
      CG.toast('已发送：' + def.name, 'success');
    }
  }

  CG.QuickActions = QuickActions;
})();
