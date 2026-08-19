#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""测试用例加载器 —— 模拟平台调用与生命周期管理（对齐 testcases.md v1.0）。

事实源：docs/PLATFORM-API.md §11 + test/testcases.md（权威）。
自动化边界：纯 gRPC + Python 以浏览器身份连 signaling WS（/ws/browser/{svc}/{inst}）
发 bound/unbound 驱动会话生命周期；真实 WebRTC（offer/QoS/StreamDead/ERR_ACQUIRE_FAILED）
列人工/浏览器端验证（M 组），不在此加载器内。

用法:
    python runner.py                       # 跑全部非连通性用例（需 signaling 在线）
    python runner.py --case reset_device   # 只跑指定用例
    python runner.py --adb 127.0.0.1:16384 # 额外跑连通性用例
    python runner.py --list                # 列出用例

前置:
    - agentd 已启动:  D:\\usexxx\\go\\versions\\1.22.5\\go\\bin\\go.exe run ./cmd/agentd --grpc-port 17890
    - signaling 运行: go run ./cmd/signaling
    - 生成的 proto:  test/py/agent_pb2*.py（scripts/gen-proto.ps1 生成）
"""
import argparse
import asyncio
import json
import os
import sys
import threading
import time

import grpc
import websockets

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "py"))
import agent_pb2 as pb  # noqa: E402
import agent_pb2_grpc as rpc  # noqa: E402

# ---------------------------------------------------------------------------
# 用例注册表（按注册顺序执行）
# ---------------------------------------------------------------------------
TESTS = []


def test(name):
    """装饰器：注册用例。返回 (name, fn)，main 按注册顺序执行。"""
    def deco(fn):
        TESTS.append((name, fn))
        return fn
    return deco


# ---------------------------------------------------------------------------
# 公共构造 / 工具
# ---------------------------------------------------------------------------

def make_global_config(signaling_url="ws://127.0.0.1:8080", service_id="demo-service",
                       adb_path="adb"):
    return pb.GlobalConfig(
        signaling_url=signaling_url,
        service_id=service_id,
        adb_path=adb_path,
        qos_interval_ms=2000,
        scrcpy=pb.ScrcpyConfig(
            server_version="4.0",
            jar_path="./scrcpy-server.jar",
            port_pool_start=30000,
            port_pool_size=100,
            max_size=1920,
            video_bit_rate=8_000_000,
            min_video_bit_rate=300_000,
            clear_bitrate=3_000_000,
            hd_bitrate=6_000_000,
            warm_keep_seconds=300,
            audio_bit_rate=256000,
            video_codec="h264",
            audio_codec="opus",
            audio_enabled=True,
            power_on=True,
            stay_awake=True,
            video_keyframe_interval=2,
        ),
    )


class AgentClient:
    def __init__(self, addr):
        self.channel = grpc.insecure_channel(addr)
        self.stub = rpc.AgentServiceStub(self.channel)


def wait_device(client, serial, predicate, timeout=30.0):
    """轮询 ListDevices 直到 predicate(DeviceStatus) 为真。"""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        resp = client.stub.ListDevices(pb.Empty())
        for d in resp.devices:
            if d.device_serial == serial:
                last = d
                if predicate(d):
                    return d
        time.sleep(0.5)
    raise AssertionError(f"device {serial} predicate not satisfied in {timeout}s, last={last}")


class EventCollector:
    """StreamEvents 订阅收集器（后台线程）。"""

    def __init__(self, client):
        self.events = []
        self._stop = threading.Event()

        def reader():
            try:
                for ev in client.stub.StreamEvents(pb.Empty()):
                    if self._stop.is_set():
                        break
                    self.events.append(ev)
            except grpc.RpcError:
                pass

        self._th = threading.Thread(target=reader, daemon=True)
        self._th.start()
        time.sleep(0.5)  # 等待订阅建立

    def clear(self):
        self.events.clear()

    def wait(self, pred, timeout=10.0):
        """等待满足 pred(ev) 的事件出现。"""
        deadline = time.time() + timeout
        while time.time() < deadline:
            for ev in self.events:
                if pred(ev):
                    return ev
            time.sleep(0.1)
        raise AssertionError(f"event predicate not satisfied in {timeout}s")

    def stop(self):
        self._stop.set()


def now_serial(prefix):
    return f"{prefix}-{int(time.time())}-{threading.get_ident()}"


# ---------------------------------------------------------------------------
# 用例：生命周期状态机（L 组）
# ---------------------------------------------------------------------------

@test("pre_init_errors")
def t_pre_init_errors(c, args):
    """L02：未 Init 调其他接口 → ERR_NOT_INIT。"""
    for name, fn, req in [
        ("PrepareDevice", c.stub.PrepareDevice, pb.PrepareDeviceRequest(device_serial="x", instance_id="y")),
        ("ListDevices", c.stub.ListDevices, pb.Empty()),
        ("Start", c.stub.Start, pb.Empty()),
        ("ReloadConfig", c.stub.ReloadConfig, pb.ReloadConfigRequest()),
        ("SetQuality", c.stub.SetQuality, pb.SetQualityRequest(device_serial="x", level="hd")),
        ("ReleaseDevice", c.stub.ReleaseDevice, pb.ReleaseDeviceRequest(device_serial="x")),
        ("ResetDevice", c.stub.ResetDevice, pb.ResetDeviceRequest(device_serial="x")),
    ]:
        resp = fn(req)
        assert resp.error_code == "ERR_NOT_INIT", f"{name}: {resp}"


@test("init_bad_config")
def t_init_bad_config(c, args):
    """L08：未 Init 时 Init 缺 global_config → ERR_INVALID_ARG。"""
    r = c.stub.Init(pb.InitRequest(grpc_listen_port=0))
    assert r.error_code == "ERR_INVALID_ARG", r


@test("init")
def t_init(c, args):
    """L01：Init 成功，actual_port=进程端口（grpc_listen_port=0 亦然）。"""
    resp = c.stub.Init(pb.InitRequest(grpc_listen_port=0, global_config=make_global_config()))
    assert resp.ok, resp
    assert resp.error_code == "OK"
    assert resp.actual_port > 0, resp


@test("init_errors")
def t_init_errors(c, args):
    """L03：重复 Init → ERR_ALREADY_INIT。"""
    r = c.stub.Init(pb.InitRequest(grpc_listen_port=0, global_config=make_global_config()))
    assert r.error_code == "ERR_ALREADY_INIT", r


@test("health")
def t_health(c, args):
    """G01：Health 字段齐全。"""
    resp = c.stub.Health(pb.Empty())
    assert resp.ok and resp.version, resp
    assert resp.device_count >= 0 and resp.uptime_seconds >= 0
    assert isinstance(resp.last_error, str)


@test("reload_config")
def t_reload_config(c, args):
    """C01：ReloadConfig 全量替换；缺 global_config → ERR_INVALID_ARG。"""
    resp = c.stub.ReloadConfig(pb.ReloadConfigRequest(global_config=make_global_config()))
    assert resp.ok, resp
    resp2 = c.stub.ReloadConfig(pb.ReloadConfigRequest())
    assert not resp2.ok and resp2.error_code == "ERR_INVALID_ARG", resp2


@test("lifecycle")
def t_lifecycle(c, args):
    """L04/L05/L06/L07：Start/Stop 幂等 + 重新初始化闭环。"""
    assert c.stub.Start(pb.Empty()).ok
    assert c.stub.Start(pb.Empty()).ok  # RUNNING 再 Start 幂等
    assert c.stub.Stop(pb.Empty()).ok
    assert c.stub.Stop(pb.Empty()).ok  # 任意状态 Stop 幂等
    # 重新初始化闭环
    r = c.stub.Init(pb.InitRequest(grpc_listen_port=0, global_config=make_global_config()))
    assert r.ok, r
    assert c.stub.Start(pb.Empty()).ok


# ---------------------------------------------------------------------------
# 用例：设备管理（D 组）
# ---------------------------------------------------------------------------

@test("prepare_release_lifecycle")
def t_prepare_release(c, args):
    """D01/D02/D03/D05/D06/D07：Prepare/ListDevices/Release 生命周期。"""
    # 缺参校验
    r = c.stub.PrepareDevice(pb.PrepareDeviceRequest(instance_id="i"))
    assert r.error_code == "ERR_INVALID_ARG", r
    r = c.stub.PrepareDevice(pb.PrepareDeviceRequest(device_serial="s"))
    assert r.error_code == "ERR_INVALID_ARG", r

    serial = now_serial("lc")
    resp = c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="lc-inst", device_serial=serial,
        device_config=pb.DeviceConfig(video_bit_rate=6_000_000),
    ))
    assert resp.ok, resp
    d = wait_device(c, serial, lambda s: s.configured)
    assert d.instance_id == "lc-inst"
    # D05：ListDevices 字段完整
    for field in ("instance_id", "device_serial", "busy", "current_session_id",
                  "connected_seconds", "signaling_connected", "configured"):
        assert hasattr(d, field), f"missing field {field}"

    # D04：已存在设备平滑更新
    assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="lc-inst", device_serial=serial,
        device_config=pb.DeviceConfig(video_bit_rate=5_000_000),
    )).ok
    d = wait_device(c, serial, lambda s: s.configured)
    assert d.configured

    collector = EventCollector(c)
    resp = c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial, reason="test"))
    assert resp.ok, resp
    time.sleep(0.3)
    listed = c.stub.ListDevices(pb.Empty())
    assert not any(x.device_serial == serial for x in listed.devices), "device not released"
    # D06b：DeviceReleased 事件 reason 透传
    ev = collector.wait(lambda e: e.device_serial == serial and e.HasField("device_released"),
                        timeout=10.0)
    assert ev.device_released.reason == "test", ev
    collector.stop()

    # D07：不存在设备
    r = c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=now_serial("nope")))
    assert r.error_code == "ERR_DEVICE_NOT_FOUND", r


@test("reset_device")
def t_reset_device(c, args):
    """D08/D09/D10：ResetDevice 保留语义（设备保留 + configured=false，可重激活）。"""
    serial = now_serial("reset")
    assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="reset-inst", device_serial=serial)).ok
    wait_device(c, serial, lambda s: s.configured)

    collector = EventCollector(c)
    resp = c.stub.ResetDevice(pb.ResetDeviceRequest(device_serial=serial, reason="config_reset"))
    assert resp.ok, resp
    time.sleep(0.3)
    d = wait_device(c, serial, lambda s: not s.configured)
    assert d.configured is False
    assert d.signaling_connected is False
    assert d.busy is False
    # D08b：DeviceReset 事件 reason 透传
    ev = collector.wait(lambda e: e.device_serial == serial and e.HasField("device_reset"),
                        timeout=10.0)
    assert ev.device_reset.reason == "config_reset", ev
    collector.stop()

    # D09：再次 Prepare 可重新激活
    assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="reset-inst", device_serial=serial)).ok
    d = wait_device(c, serial, lambda s: s.configured)
    assert d.configured
    c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))

    # D10：不存在设备
    r = c.stub.ResetDevice(pb.ResetDeviceRequest(device_serial=now_serial("nope2")))
    assert r.error_code == "ERR_DEVICE_NOT_FOUND", r


@test("set_quality")
def t_set_quality(c, args):
    """E04/E05：三档 ok；无效 level → ERR_INVALID_ARG；不存在 → ERR_DEVICE_NOT_FOUND。"""
    r = c.stub.SetQuality(pb.SetQualityRequest(device_serial=now_serial("nope3"), level="hd"))
    assert r.error_code == "ERR_DEVICE_NOT_FOUND", r

    serial = now_serial("q")
    assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="q-inst", device_serial=serial)).ok
    wait_device(c, serial, lambda s: s.configured)
    for lv in ("clear", "hd", "original"):
        r = c.stub.SetQuality(pb.SetQualityRequest(device_serial=serial, level=lv))
        assert r.ok, r
    r = c.stub.SetQuality(pb.SetQualityRequest(device_serial=serial, level="ultra"))
    assert r.error_code == "ERR_INVALID_ARG", r
    c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))


@test("send_control")
def t_send_control(c, args):
    """E06：缺 control → ERR_INVALID_ARG；设备不存在 → ERR_DEVICE_NOT_FOUND；ok=fire-and-forget。"""
    r = c.stub.SendControl(pb.SendControlRequest(device_serial=now_serial("nope4")))
    assert r.error_code == "ERR_DEVICE_NOT_FOUND", r

    serial = now_serial("sc")
    assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="sc-inst", device_serial=serial)).ok
    wait_device(c, serial, lambda s: s.configured)
    r = c.stub.SendControl(pb.SendControlRequest(device_serial=serial))
    assert r.error_code == "ERR_INVALID_ARG", r
    r = c.stub.SendControl(pb.SendControlRequest(
        device_serial=serial,
        control=pb.ControlMessage(type="rotate_device", data={}),
    ))
    assert r.ok, r
    c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))


@test("force_close_session")
def t_force_close(c, args):
    """E03：无活动会话时任意 session_id → ERR_SESSION_NOT_FOUND；空参 → ERR_INVALID_ARG。"""
    r = c.stub.ForceCloseSession(pb.ForceCloseSessionRequest())
    assert r.error_code == "ERR_INVALID_ARG", r
    r = c.stub.ForceCloseSession(pb.ForceCloseSessionRequest(session_id="ghost"))
    assert r.error_code == "ERR_SESSION_NOT_FOUND", r


# ---------------------------------------------------------------------------
# 用例：事件流（F 组）
# ---------------------------------------------------------------------------

@test("stream_events")
def t_stream_events(c, args):
    """F01/F04：订阅后 PrepareDevice 收到 DeviceStatusChanged；断线不重放。"""
    collector = EventCollector(c)
    try:
        serial = now_serial("ev")
        assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
            instance_id="ev-inst", device_serial=serial)).ok
        ev = collector.wait(lambda e: e.device_serial == serial and e.HasField("device_status"))
        assert ev.device_status.busy is False, ev
        assert ev.timestamp_ms > 0

        # F04：断线不重放 —— 新订阅者收不到旧事件，以 ListDevices 补偿
        collector2 = EventCollector(c)
        time.sleep(0.3)
        assert not any(e.device_serial == serial for e in collector2.events), "replayed old events"
        listed = c.stub.ListDevices(pb.Empty())
        assert any(x.device_serial == serial for x in listed.devices)
        collector2.stop()

        c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))
    finally:
        collector.stop()


@test("multi_subscribe")
def t_multi_subscribe(c, args):
    """F02：多订阅 fan-out，两者都收到同一事件。"""
    a, b = EventCollector(c), EventCollector(c)
    try:
        serial = now_serial("fan")
        assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
            instance_id="fan-inst", device_serial=serial)).ok
        a.wait(lambda e: e.device_serial == serial and e.HasField("device_status"))
        b.wait(lambda e: e.device_serial == serial and e.HasField("device_status"))
        c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))
    finally:
        a.stop()
        b.stop()


# ---------------------------------------------------------------------------
# 用例：会话生命周期（E 组，Python 模拟浏览器 bound/unbound）
# ---------------------------------------------------------------------------

async def await_event_async(collector, pred, timeout=10.0):
    """async 版事件等待（供单 loop 的 WS 流程内轮询 collector.events）。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        for ev in collector.events:
            if pred(ev):
                return ev
        await asyncio.sleep(0.1)
    raise AssertionError(f"event predicate not satisfied in {timeout}s")


async def browser_flow_connect(service_id, instance_id, base="ws://127.0.0.1:8080"):
    """单 loop 内连接浏览器 WS 并消费首条 ice_servers 消息。"""
    ws = await websockets.connect(f"{base}/ws/browser/{service_id}/{instance_id}")
    await ws.recv()  # ice_servers
    return ws


@test("session_lifecycle")
def t_session_lifecycle(c, args):
    """E01：bound → SessionStarted；断开 → SessionStopped(user_unbound)。"""
    collector = EventCollector(c)
    try:
        serial = now_serial("sess")
        assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
            instance_id="sess-inst", device_serial=serial)).ok
        # 等信令连接建立
        wait_device(c, serial, lambda s: s.signaling_connected, timeout=15.0)

        async def flow():
            ws = await browser_flow_connect("demo-service", "sess-inst")
            try:
                ev = await await_event_async(
                    collector,
                    lambda e: e.device_serial == serial and e.HasField("session_started"))
                return ev.session_started.session_id
            finally:
                await ws.close()

        sid = asyncio.run(flow())
        assert sid

        ev = collector.wait(lambda e: e.device_serial == serial and e.HasField("session_stopped"),
                            timeout=10.0)
        assert ev.session_stopped.reason == "user_unbound", ev
        c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))
    finally:
        collector.stop()


@test("force_close_forced")
def t_force_close_forced(c, args):
    """E02：ForceCloseSession → SessionStopped(forced) + 浏览器收到 preempted。"""
    collector = EventCollector(c)
    try:
        serial = now_serial("fc2")
        assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
            instance_id="fc2-inst", device_serial=serial)).ok
        wait_device(c, serial, lambda s: s.signaling_connected, timeout=15.0)

        async def flow():
            ws = await browser_flow_connect("demo-service", "fc2-inst")
            try:
                ev = await await_event_async(
                    collector,
                    lambda e: e.device_serial == serial and e.HasField("session_started"))
                sid = ev.session_started.session_id
                assert sid, ev

                resp = c.stub.ForceCloseSession(pb.ForceCloseSessionRequest(session_id=sid, reason="test"))
                assert resp.ok, resp

                # 浏览器收到 preempted（signaling 随后关闭 WS）
                raw = await asyncio.wait_for(ws.recv(), 5.0)
                msg = json.loads(raw)
                assert msg.get("type") == "preempted", msg
                return sid
            finally:
                await ws.close()

        sid = asyncio.run(flow())
        assert sid

        ev = collector.wait(lambda e: e.device_serial == serial and e.HasField("session_stopped"),
                            timeout=10.0)
        assert ev.session_stopped.reason == "test", ev
        c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))
    finally:
        collector.stop()


# ---------------------------------------------------------------------------
# 用例：信令故障（F05）
# ---------------------------------------------------------------------------

@test("signaling_fault")
def t_signaling_fault(c, args):
    """F05：不可达 signaling_url → AgentError{ERR_SIGNALING_DISCONNECTED} + signaling_connected=false。"""
    collector = EventCollector(c)
    try:
        serial = now_serial("sig")
        # 切到不可达地址，Prepare 后连接失败
        assert c.stub.ReloadConfig(pb.ReloadConfigRequest(global_config=make_global_config(
            signaling_url="ws://127.0.0.1:1", service_id="bad-svc"))).ok
        assert c.stub.PrepareDevice(pb.PrepareDeviceRequest(
            instance_id="sig-inst", device_serial=serial)).ok

        ev = collector.wait(lambda e: e.device_serial == serial and e.HasField("agent_error"),
                            timeout=10.0)
        assert ev.agent_error.code == "ERR_SIGNALING_DISCONNECTED", ev

        d = wait_device(c, serial, lambda s: s.signaling_connected is False)
        assert d.signaling_connected is False
        c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial))

        # 恢复全局配置供后续用例
        assert c.stub.ReloadConfig(pb.ReloadConfigRequest(global_config=make_global_config())).ok
    finally:
        collector.stop()


# ---------------------------------------------------------------------------
# 用例：连通性（H 组，需真实 adb 设备）
# ---------------------------------------------------------------------------

@test("connectivity")
def t_connectivity(c, args):
    """H01：真实设备 Prepare → signaling_connected=true → Health → Release。"""
    serial = args.adb
    assert serial, "--adb 参数必填"
    resp = c.stub.PrepareDevice(pb.PrepareDeviceRequest(
        instance_id="conn-test", device_serial=serial))
    assert resp.ok, resp
    d = wait_device(c, serial, lambda s: s.signaling_connected, timeout=args.conn_timeout)
    print(f"  -> signaling connected: {d.signaling_connected}")
    h = c.stub.Health(pb.Empty())
    assert h.device_count >= 1, h
    assert c.stub.ReleaseDevice(pb.ReleaseDeviceRequest(device_serial=serial)).ok


# ---------------------------------------------------------------------------
# 加载器主流程
# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(description="Agent sidecar 测试用例加载器")
    ap.add_argument("--grpc", default="127.0.0.1:17890", help="agentd gRPC 地址")
    ap.add_argument("--adb", default=None, help="adb 设备序列号（连通性测试）")
    ap.add_argument("--case", default=None, help="只运行指定用例名")
    ap.add_argument("--list", action="store_true", help="列出所有用例")
    ap.add_argument("--conn-timeout", type=float, default=30.0, help="连通性等待超时（秒）")
    args = ap.parse_args()

    if args.list:
        for name, _ in TESTS:
            print(name)
        return 0

    client = AgentClient(args.grpc)
    names = [args.case] if args.case else [n for n, _ in TESTS]
    if args.case and args.case not in [n for n, _ in TESTS]:
        print(f"unknown case: {args.case}")
        return 2

    passed, failed = [], []
    for name in names:
        fn = dict(TESTS)[name]
        if name == "connectivity" and not args.adb:
            print(f"  [SKIP] {name}  (需要 --adb 参数)")
            continue
        print(f"  [RUN ] {name}", end="", flush=True)
        try:
            fn(client, args)
            print(f"\r  [PASS] {name}")
            passed.append(name)
        except Exception as e:  # noqa: BLE001
            import traceback
            print(f"\r  [FAIL] {name}  ->  {type(e).__name__}: {e}")
            traceback.print_exc()
            failed.append(name)

    print("\n================== 结果汇总 ==================")
    print(f"  通过: {len(passed)}  失败: {len(failed)}")
    for n in failed:
        print(f"  FAILED: {n}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
