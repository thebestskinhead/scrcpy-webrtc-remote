const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

function load() {
  const timers = new Map();
  let next = 0;
  class Target {
    constructor() { this.listeners = new Map(); this.tabIndex = -1; this.style = {}; }
    addEventListener(type, fn) { if (!this.listeners.has(type)) this.listeners.set(type, new Set()); this.listeners.get(type).add(fn); }
    removeEventListener(type, fn) { this.listeners.get(type)?.delete(fn); }
    count() { return [...this.listeners.values()].reduce((n, s) => n + s.size, 0); }
    focus() {}
  }
  const window = new Target();
  const media = new Target();
  window.matchMedia = () => media;
  const sockets = [];
  class Socket extends Target {
    constructor(url, protocols) { super(); this.url = url; this.protocols = protocols; sockets.push(this); }
    close() { this.closed = true; }
  }
  Socket.OPEN = 1;
  const context = { window, console: { log() {} }, location: { host: 'localhost', protocol: 'http:' }, URL, URLSearchParams,
    performance, document: { getElementById() { return null; } }, WebSocket: Socket,
    setTimeout(fn) { const id = ++next; timers.set(id, fn); return id; }, clearTimeout(id) { timers.delete(id); },
    setInterval(fn) { const id = ++next; timers.set(id, fn); return id; }, clearInterval(id) { timers.delete(id); },
    ResizeObserver: class { observe() {} disconnect() { this.closed = true; } },
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../signaling/static/app/js/cg-sdk.js'), 'utf8'), context);
  return { CG: window.CG, Target, window, media, timers, sockets };
}

function authorization() {
  return { token: 'secret', session: { session_id: 'id', service_id: 'svc', instance_id: 'inst', expires_at: new Date(Date.now() + 60000).toISOString() } };
}

test('sessions isolate events and once removes before reentry', () => {
  const { CG } = load();
  const a = new CG.RemoteSession();
  const b = new CG.RemoteSession();
  let left = 0, right = 0;
  a.on('state-change', () => left++);
  b.on('state-change', () => right++);
  a.connection._setState('connected');
  assert.equal(left, 1);
  assert.equal(right, 0);
  let once = 0;
  a.once('custom', () => { once++; a.events.emit('custom'); });
  a.events.emit('custom');
  assert.equal(once, 1);
  a.destroy(); a.destroy(); b.destroy();
});

test('canceling pending handshake settles connect and removes timers', async () => {
  const { CG, sockets, timers } = load();
  const session = new CG.RemoteSession({ authorization: authorization(), signalingBase: 'https://example.test' });
  const pending = session.connect('svc', 'inst');
  assert.equal(sockets.length, 1);
  assert.equal(new URL(sockets[0].url).protocol, 'wss:');
  assert.equal(new URL(sockets[0].url).searchParams.get('session_id'), 'id');
  assert.ok(!sockets[0].url.includes('secret'));
  assert.equal(sockets[0].protocols[0], 'astra.auth.secret');
  session.destroy();
  await pending;
  assert.equal(sockets[0].closed, true);
  assert.equal(timers.size, 0);
  await assert.rejects(session.connect('svc', 'inst'));
});

test('input listeners are scoped and removed on destroy and rebind', () => {
  const { CG, Target, window, media, timers } = load();
  const overlay = new Target();
  const input = new CG.InputController();
  const conn = new CG.ConnectionManager();
  input.bind(conn, { overlayEl: overlay });
  assert.ok(overlay.listeners.has('keydown'));
  assert.equal(window.listeners.has('keydown'), false);
  assert.ok(timers.size > 0);
  input.destroy(); input.destroy();
  assert.equal(overlay.count(), 0);
  assert.equal(window.count(), 0);
  assert.equal(media.count(), 0);
  assert.equal(timers.size, 0);
  input.bind(conn, { overlayEl: overlay });
  assert.ok(overlay.count() > 0);
  input.destroy(); conn.destroy();
});

test('late stats and ICE completion do not reactivate canceled sessions', async () => {
  const { CG, timers } = load();
  const conn = new CG.ConnectionManager();
  const pending = conn._waitForICEServers();
  conn.disconnect(true);
  await pending;
  assert.equal(timers.size, 0);
  let resolve;
  conn.pc = { getStats: () => new Promise(r => { resolve = r; }) };
  const stats = new CG.StatsMonitor();
  stats.start(conn);
  const sampling = stats._tick();
  stats.stop();
  resolve(new Map());
  await sampling;
  conn.pc = null;
  conn.destroy();
  assert.equal(timers.size, 0);
});

test('missing authorization fails closed and relay cannot preempt', async () => {
  const { CG, sockets } = load();
  const conn = new CG.ConnectionManager();
  await conn.connect('svc', 'inst');
  assert.equal(conn.state, 'error');
  assert.equal(sockets.length, 0);
  await assert.rejects(CG.relayQuery('svc', 'inst', { type: 'preempt' }, { authorization: authorization() }));
  conn.destroy();
});
