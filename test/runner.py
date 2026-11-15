#!/usr/bin/env python3
"""HTTP/SSE platform integration checks. Run against a disposable agentd instance.

Requires ASTRA_MANAGEMENT_TOKEN. Does not inject media offers or operate ADB.
Use --adb explicitly to include device registration checks (not media acceptance).
"""
import argparse
import asyncio
import json
import os
import queue
import sys
import threading
import time
import uuid
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, build_opener, HTTPRedirectHandler

TESTS = {}


def test(name):
    def register(fn):
        TESTS[name] = fn
        return fn
    return register


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class AgentClient:
    def __init__(self, endpoint, token):
        self.endpoint = endpoint.rstrip('/')
        self.token = token
        self.opener = build_opener(NoRedirect)

    def open(self, method, path, data=None, token=None, timeout=15):
        body = None if data is None else json.dumps(data).encode()
        request = Request(self.endpoint + path, data=body, method=method, headers={
            'Authorization': 'Bearer ' + (self.token if token is None else token),
            'Content-Type': 'application/json',
        })
        try:
            return self.opener.open(request, timeout=timeout)
        except HTTPError as error:
            return error

    def call(self, method, path, data=None, status=200, token=None):
        with self.open(method, path, data, token) as response:
            result = json.load(response)
            assert response.status == status, (path, response.status, result)
            if status == 200 and 'ok' in result:
                assert result['ok'], result
            return result


def config(args):
    return {'signaling_url': args.signaling, 'service_id': 'http-test',
            'scrcpy': {'audio_enabled': False}}


def device_path(serial, action):
    return '/v1/devices/' + quote(serial, safe='') + '/' + action


def prepare(client, serial):
    return client.call('POST', '/v1/devices', {'device_serial': serial, 'instance_id': serial})


def wait_device(client, serial, predicate, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for device in client.call('GET', '/v1/devices')['devices']:
            if device['device_serial'] == serial and predicate(device):
                return device
        time.sleep(.1)
    raise AssertionError('device condition timed out: ' + serial)


class EventCollector:
    def __init__(self, client):
        self.events = queue.Queue()
        self.response = client.open('GET', '/v1/events', timeout=3)
        assert self.response.status == 200
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self.read, daemon=True)
        self.thread.start()

    def read(self):
        try:
            while not self.stopped.is_set():
                line = self.response.readline()
                if not line:
                    return
                if line.startswith(b'data: '):
                    self.events.put(json.loads(line[6:]))
        except (OSError, ValueError):
            return

    def wait(self, kind, serial, timeout=3):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                event = self.events.get(timeout=max(.01, deadline-time.monotonic()))
            except queue.Empty:
                break
            if event['type'] == kind and event['device_serial'] == serial:
                return event
        raise AssertionError('missing event ' + kind)

    def stop(self):
        self.stopped.set()
        self.thread.join(timeout=4)
        self.response.close()
        assert not self.thread.is_alive(), 'SSE reader leaked'


@test('authentication')
def authentication(client, args):
    client.call('GET', '/v1/health', status=401, token='invalid')
    client.call('POST', '/v1/sessions', {}, status=401, token='invalid')


@test('lifecycle')
def lifecycle(client, args):
    client.call('GET', '/v1/devices', status=409)
    client.call('POST', '/v1/start', status=409)
    client.call('POST', '/v1/init', {}, status=400)
    result = client.call('POST', '/v1/init', {'global_config': config(args)})
    assert result['actual_port'] > 0
    client.call('POST', '/v1/init', {'global_config': config(args)}, status=409)
    client.call('POST', '/v1/start')
    client.call('POST', '/v1/start')
    assert client.call('GET', '/v1/health')['version']
    client.call('PUT', '/v1/config', {'global_config': config(args)})
    client.call('PUT', '/v1/config', {}, status=400)
    client.call('POST', '/v1/stop')
    client.call('POST', '/v1/stop')


@test('device_management')
def device_management(client, args):
    serial = 'http-test-' + uuid.uuid4().hex
    prepare(client, serial)
    try:
        device = wait_device(client, serial, lambda value: value['configured'])
        assert device['instance_id'] == serial
        client.call('POST', '/v1/devices', {'device_serial': serial}, status=400)
        client.call('POST', '/v1/devices', {'device_serial': serial, 'instance_id': 'other'}, status=409)
        for level in ('clear', 'hd', 'original'):
            client.call('PUT', device_path(serial, 'quality'), {'level': level})
        client.call('PUT', device_path(serial, 'quality'), {'level': 'invalid'}, status=400)
        client.call('POST', device_path(serial, 'control'), {}, status=400)
        client.call('POST', device_path(serial, 'reset'), {'reason': 'test'})
        wait_device(client, serial, lambda value: not value['configured'])
        prepare(client, serial)
    finally:
        client.call('POST', device_path(serial, 'release'), {'reason': 'test'})
    client.call('POST', device_path(serial, 'release'), {}, status=404)
    client.call('POST', '/v1/sessions/missing/close', {}, status=404)


@test('sse')
def sse(client, args):
    serial = 'sse-' + uuid.uuid4().hex
    collectors = [EventCollector(client), EventCollector(client)]
    try:
        prepare(client, serial)
        for collector in collectors:
            event = collector.wait('device_status', serial)
            assert event['timestamp_ms'] > 0
        client.call('POST', device_path(serial, 'reset'), {'reason': 'reset'})
        for collector in collectors:
            assert collector.wait('device_reset', serial)['data']['reason'] == 'reset'
        client.call('POST', device_path(serial, 'release'), {'reason': 'release'})
        for collector in collectors:
            assert collector.wait('device_released', serial)['data']['reason'] == 'release'
    finally:
        for collector in collectors:
            collector.stop()


@test('authorized_sessions')
def authorized_sessions(client, args):
    serial = 'grant-' + uuid.uuid4().hex
    prepare(client, serial)
    def issue(preempt=False):
        return client.call('POST', '/v1/sessions', {'device_serial': serial, 'preempt': preempt})
    def validate(auth, status=200):
        grant = auth['session']
        return client.call('POST', '/v1/sessions/validate', {
            'session_id': grant['session_id'], 'token': auth['token'],
            'service_id': grant['service_id'], 'instance_id': grant['instance_id'],
        }, status=status)
    try:
        first = issue()
        validate(first)
        client.call('POST', '/v1/sessions', {'device_serial': serial}, status=409)
        second = issue(True)
        validate(first, 401)
        validate(second)
        client.call('DELETE', '/v1/sessions/' + second['session']['session_id'])
        validate(second, 401)
        third = issue()
        client.call('POST', device_path(serial, 'reset'), {})
        validate(third, 401)
    finally:
        client.call('POST', device_path(serial, 'release'), {})


@test('browser_lifecycle')
def browser_lifecycle(client, args):
    import websockets
    serial = 'browser-' + uuid.uuid4().hex
    prepare(client, serial)
    try:
        wait_device(client, serial, lambda value: value['signaling_connected'])
        auth = client.call('POST', '/v1/sessions', {'device_serial': serial})
        grant = auth['session']
        async def flow():
            url = args.signaling.rstrip('/') + '/ws/browser/http-test/' + serial + '?session_id=' + grant['session_id']
            async with websockets.connect(url, subprotocols=['astra.auth.' + auth['token']]) as ws:
                assert json.loads(await asyncio.wait_for(ws.recv(), 5))['type'] == 'ice_servers'
                wait_device(client, serial, lambda value: value['current_session_id'] == grant['session_id'])
                client.call('DELETE', '/v1/sessions/' + grant['session_id'])
                try:
                    while True:
                        await asyncio.wait_for(ws.recv(), 5)
                except websockets.exceptions.ConnectionClosed:
                    pass
        asyncio.run(flow())
        wait_device(client, serial, lambda value: not value['busy'])
    finally:
        client.call('POST', device_path(serial, 'release'), {})


@test('connectivity')
def connectivity(client, args):
    prepare(client, args.adb)
    try:
        wait_device(client, args.adb, lambda value: value['signaling_connected'])
    finally:
        client.call('POST', device_path(args.adb, 'release'), {})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--http', default='http://127.0.0.1:17890')
    parser.add_argument('--signaling', default='ws://127.0.0.1:8080')
    parser.add_argument('--case', choices=list(TESTS))
    parser.add_argument('--list', action='store_true')
    parser.add_argument('--adb', help='explicit device serial for registration-only check')
    args = parser.parse_args()
    if args.list:
        print('\n'.join(TESTS))
        return 0
    token = os.environ.get('ASTRA_MANAGEMENT_TOKEN')
    if not token:
        parser.error('ASTRA_MANAGEMENT_TOKEN is required')
    client = AgentClient(args.http, token)
    failed = []
    for name in [args.case] if args.case else TESTS:
        if name == 'connectivity' and not args.adb:
            print('[SKIP]', name)
            continue
        try:
            client.call('POST', '/v1/stop')
            if name not in ('authentication', 'lifecycle'):
                client.call('POST', '/v1/init', {'global_config': config(args)})
                client.call('POST', '/v1/start')
            TESTS[name](client, args)
            print('[PASS]', name)
        except Exception as error:
            failed.append(name)
            print('[FAIL]', name, repr(error))
        finally:
            client.call('POST', '/v1/stop')
    return int(bool(failed))


if __name__ == '__main__':
    sys.exit(main())
