#!/usr/bin/env python3
"""Isolated, explicitly authorized device E2E. Audio is always disabled."""
import argparse
import contextlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time

from runner import AgentClient, device_path, wait_device

ROOT = Path(__file__).resolve().parents[1]


def run(*args, **kwargs):
    return subprocess.run(args, check=True, timeout=180, cwd=ROOT, **kwargs)


@contextlib.contextmanager
def runtime():
    with tempfile.TemporaryDirectory(prefix='astra-e2e-') as directory:
        root = Path(directory)
        env = os.environ.copy()
        env.pop('ASTRA_TEST_DEVICE', None)
        env.pop('ASTRA_HTTP_PORT', None)
        env['ASTRA_MANAGEMENT_TOKEN'] = secrets.token_urlsafe(32)
        env['ASTRA_AGENT_TOKEN'] = secrets.token_urlsafe(32)
        with socket.socket() as a, socket.socket() as b:
            a.bind(('127.0.0.1', 0))
            b.bind(('127.0.0.1', 0))
            hp, sp = a.getsockname()[1], b.getsockname()[1]
            for name in ('agentd', 'signaling'):
                run('go', 'build', '-o', str(root / name), './cmd/' + name)
        http = f'http://127.0.0.1:{hp}'
        ws = f'ws://127.0.0.1:{sp}'
        config = root / 'signaling.yaml'
        config.write_text(f'host: 127.0.0.1\nport: {sp}\nstatic_dir: {ROOT}/signaling/static\n')
        processes = []
        with (root / 'services.log').open('w+') as log:
            try:
                for command in ([str(root / 'agentd'), '--http-port', str(hp)],
                                [str(root / 'signaling'), '-c', str(config), '--management-url', http]):
                    processes.append(subprocess.Popen(command, cwd=ROOT, env=env, stdout=log, stderr=log))
                for port in (hp, sp):
                    deadline = time.monotonic() + 20
                    while True:
                        if any(p.poll() is not None for p in processes):
                            raise RuntimeError('test service exited during startup')
                        try:
                            with socket.create_connection(('127.0.0.1', port), .2):
                                break
                        except OSError:
                            if time.monotonic() > deadline:
                                raise TimeoutError('test service startup')
                            time.sleep(.1)
                client = AgentClient(http, env['ASTRA_MANAGEMENT_TOKEN'])
                yield client, http, ws, env
            except Exception:
                log.flush()
                log.seek(0)
                lines = [line for line in log if any(word in line.lower() for word in ('error', 'failed', 'timeout', 'audio disabled'))]
                for line in lines[-20:]:
                    print('[service]', line.strip(), flush=True)
                raise
            finally:
                for p in processes:
                    p.terminate()
                for p in processes:
                    try:
                        p.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        p.kill()
                        p.wait()


def signaling_checks(http, ws, env, serial):
    for case in ('browser_lifecycle', 'connectivity'):
        run(sys.executable, str(ROOT / 'test/runner.py'), '--case', case,
            '--http', http, '--signaling', ws, '--adb', serial, env=env)


def adb(serial, *args):
    return run('adb', '-s', serial, *args, capture_output=True, text=True).stdout


def focus(serial):
    output = adb(serial, 'shell', 'dumpsys', 'activity', 'activities')
    return next((line.strip() for line in output.splitlines() if 'topResumedActivity=' in line), '')


def display_settings(serial):
    adb(serial, 'shell', 'input', 'keyevent', 'KEYCODE_WAKEUP')
    adb(serial, 'shell', 'am', 'start', '-W', '-a', 'android.settings.DISPLAY_SETTINGS')
    deadline = time.monotonic() + 15
    while True:
        current = focus(serial)
        if 'settings' in current.lower():
            return current
        if time.monotonic() > deadline:
            raise AssertionError('display settings did not gain focus: ' + current)
        time.sleep(.5)


def media_stats(page):
    return page.evaluate('''async () => {
        const c = window._app.conn, v = document.getElementById('remoteVideo');
        const stats = [...(await c.pc.getStats()).values()];
        const video = stats.filter(s => s.type === 'inbound-rtp' && s.kind === 'video');
        const audio = stats.filter(s => s.type === 'inbound-rtp' && s.kind === 'audio');
        const canvas = document.createElement('canvas'); canvas.width = 32; canvas.height = 32;
        const ctx = canvas.getContext('2d'); ctx.drawImage(v, 0, 0, 32, 32);
        const pixels = ctx.getImageData(0, 0, 32, 32).data;
        let min = 255, max = 0, hash = 0;
        for (let i = 0; i < pixels.length; i += 4) {
            min = Math.min(min, pixels[i]); max = Math.max(max, pixels[i]);
            hash = ((hash * 31) + pixels[i] + pixels[i+1] + pixels[i+2]) >>> 0;
        }
        return {frames: video.reduce((sum, s) => sum + (s.framesDecoded || 0), 0),
            bytes: video.reduce((sum, s) => sum + (s.bytesReceived || 0), 0),
            width: v.videoWidth, height: v.videoHeight, pixelRange: max-min, hash,
            presented: v.getVideoPlaybackQuality().totalVideoFrames,
            audioBytes: audio.reduce((sum, s) => sum + (s.bytesReceived || 0), 0)};
    }''')


def connect_page(browser, client, ws, serial):
    auth = client.call('POST', '/v1/sessions', {'device_serial': serial, 'ttl_seconds': 300})
    page = browser.new_page()
    page.on('pageerror', lambda error: print('[page error]', str(error), flush=True))
    page.on('console', lambda message: print('[browser]', message.text, flush=True) if message.type == 'error' or '[CM]' in message.text else None)
    page.goto(ws.replace('ws://', 'http://') + '/app/?s=video-e2e&d=e2e-device')
    page.evaluate('(a) => { window.AstraSessionAuthorization = a; }', auth)
    page.locator('#connectBtn').click()
    page.wait_for_function('''() => {
        const c = window._app?.conn, v = document.getElementById('remoteVideo');
        return c?.pc?.connectionState === 'connected' && c.dc?.readyState === 'open'
            && v.readyState >= 2 && v.videoWidth > 0;
    }''', timeout=90000)
    page.wait_for_function('''async () => {
        const stats = await window._app.conn.pc.getStats();
        return [...stats.values()].some(s => s.type === 'inbound-rtp'
            && s.kind === 'video' && s.framesDecoded > 2);
    }''', timeout=30000)
    snapshot = media_stats(page)
    assert snapshot['pixelRange'] > 10 and snapshot['presented'] > 0, snapshot
    assert snapshot['audioBytes'] == 0, 'unexpected audio reception'
    print('[PASS] browser video', json.dumps(snapshot), flush=True)
    return page, auth


def control_check(page, serial, key):
    previous = display_settings(serial)
    page.wait_for_timeout(1500)
    before = media_stats(page)
    page.evaluate('''async key => {
        const c = window._app.conn;
        if (c.dc?.readyState !== 'open') throw new Error('control channel closed');
        c.sendKeyEvent(0, key);
        await new Promise(resolve => setTimeout(resolve, 50));
        c.sendKeyEvent(1, key);
    }''', key)
    deadline = time.monotonic() + 10
    while focus(serial) == previous:
        if time.monotonic() > deadline:
            raise AssertionError('browser key did not change device focus')
        page.wait_for_timeout(250)
    page.wait_for_timeout(1500)
    after = media_stats(page)
    assert after['frames'] > before['frames'] and after['hash'] != before['hash'], (before, after)
    assert after['audioBytes'] == 0
    print('[PASS] browser key', key, 'device focus changed; decoded frames', before['frames'], '->', after['frames'], flush=True)


def browser_checks(client, ws, serial):
    from playwright.sync_api import sync_playwright
    baseline = adb(serial, 'forward', '--list')
    client.call('POST', '/v1/init', {'global_config': {
        'signaling_url': ws, 'service_id': 'video-e2e', 'adb_path': 'adb',
        'scrcpy': {'jar_path': str(ROOT / 'scrcpy-server.jar'), 'server_version': '4.0',
                   'audio_enabled': False, 'max_size': 1280, 'video_bit_rate': 4000000,
                   'video_keyframe_interval': 2, 'power_on': True}}})
    client.call('POST', '/v1/start')
    try:
        client.call('POST', '/v1/devices', {'device_serial': serial, 'instance_id': 'e2e-device',
                                         'device_config': {'audio_enabled': False}})
        wait_device(client, serial, lambda d: d['signaling_connected'])
        display_settings(serial)
        with sync_playwright() as p:
            executable = os.environ.get('ASTRA_E2E_CHROME')
            browser = p.chromium.launch(headless=True, executable_path=executable)
            try:
                probe = browser.new_page()
                assert probe.evaluate("RTCRtpReceiver.getCapabilities('video').codecs.some(c => c.mimeType === 'video/H264')"), 'Browser lacks H.264; set ASTRA_E2E_CHROME to a compatible Chrome executable'
                probe.close()
                print('[INFO] browser', browser.version, flush=True)
                page, auth = connect_page(browser, client, ws, serial)
                control_check(page, serial, 4)
                control_check(page, serial, 3)
                page.close()
                wait_device(client, serial, lambda d: not d['busy'])
                client.call('DELETE', '/v1/sessions/' + auth['session']['session_id'])
                display_settings(serial)
                page, auth = connect_page(browser, client, ws, serial)
                print('[PASS] reconnect and video decode', flush=True)
                page.evaluate('''() => { window.e2ePC = window._app.conn.pc;
                    window.e2eDC = window._app.conn.dc; window.e2eWS = window._app.conn.ws; }''')
                client.call('DELETE', '/v1/sessions/' + auth['session']['session_id'])
                page.wait_for_function("() => window.e2ePC.connectionState === 'closed' && window.e2eDC.readyState === 'closed'", timeout=15000)
                wait_device(client, serial, lambda d: not d['busy'])
                print('[PASS] authorization revocation closes media/control', flush=True)
                page.close()
            finally:
                browser.close()
        client.call('POST', device_path(serial, 'release'), {})
        assert not client.call('GET', '/v1/devices')['devices'], 'device survived release'
        assert adb(serial, 'forward', '--list') == baseline, 'ADB forwards leaked'
        print('[PASS] release and ADB forward cleanup', flush=True)
    finally:
        client.call('POST', '/v1/stop')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--adb', required=True, help='explicitly authorized device serial')
    parser.add_argument('--phase', choices=('signaling', 'browser'), default='browser')
    args = parser.parse_args()
    run('adb', '-s', args.adb, 'get-state')
    with runtime() as (client, http, ws, env):
        if args.phase == 'signaling':
            signaling_checks(http, ws, env, args.adb)
        else:
            browser_checks(client, ws, args.adb)


if __name__ == '__main__':
    main()
