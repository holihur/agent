#!/usr/bin/env python3
"""agent-mcp 黑盒测试:把服务器当纯黑盒,仅经 HTTP/MCP 协议打接口。

用法(测试脚本自行编译并启动服务器):

    python3 examples/agent-mcp/blackbox_test.py

覆盖:
  - 传输层:POST-only(405)、坏帧(400)、通知(202 空体)
  - 协议层:server/discover、tools/list、未知方法(-32601)、未知工具/坏参数(-32602)
  - 错误路径:无凭据时 agent_run 以 isError 结果回填(不打断协议)
  - 会话:Mcp-Session-Id 回显
  - 可选真实调用:检测到 .env 凭据时做一次 agent_run + 同会话多轮
仅用标准库。
"""

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
PORT = 18923
URL = f"http://127.0.0.1:{PORT}/mcp"


def load_dotenv(path):
    env = {}
    if not os.path.exists(path):
        return env
    for line in open(path, encoding="utf-8"):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def post(payload, raw=None, port=PORT):
    data = raw if raw is not None else json.dumps(payload).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{port}/mcp", data=data,
                                 method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return resp.status, dict(resp.headers), resp.read()


def call(method, params=None, id=1, port=PORT):
    frame = {"jsonrpc": "2.0", "id": id, "method": method}
    if params is not None:
        frame["params"] = params
    status, headers, body = post(frame, port=port)
    assert status == 200, f"status={status}"
    assert headers.get("Content-Type") == "text/event-stream", headers
    # SSE 帧解析: event: message\ndata: {...}
    assert body.startswith(b"event: message\ndata: ") and body.endswith(b"\n\n"), body
    return headers, json.loads(body[len(b"event: message\ndata: "):-2])


def result_of(frame):
    assert "error" not in frame or frame["error"] is None, frame
    return frame["result"]


_shared_proc = None


def setUpModule():
    """传输/协议测试共用一台服务器:有凭据带凭据,无凭据也能测协议。"""
    global _shared_proc
    env = load_dotenv(os.path.join(ROOT, ".env"))
    creds = {k: env.get(k) or os.environ.get(k, "")
             for k in ("LLM_API_KEY", "LLM_APIKEY", "LLM_BASE_URL", "LLM_MODEL")}
    _shared_proc = spawn(creds)


def tearDownModule():
    global _shared_proc
    if _shared_proc:
        stop(_shared_proc)
        _shared_proc = None


class TestTransport(unittest.TestCase):
    """传输层黑盒:不经 JSON-RPC 语义,直接打 HTTP。"""

    def test_get_not_allowed(self):
        try:
            urllib.request.urlopen(URL, timeout=10)
            self.fail("expected HTTP 405")
        except urllib.error.HTTPError as e:
            self.assertEqual(e.code, 405)

    def test_bad_frame_400(self):
        try:
            post(None, raw=b"{not json")
            self.fail("expected HTTP 400")
        except urllib.error.HTTPError as e:
            self.assertEqual(e.code, 400)

    def test_notification_202_empty(self):
        status, _, body = post({"jsonrpc": "2.0", "id": None,
                                "method": "tools/list", "params": {}})
        self.assertEqual(status, 202)
        self.assertEqual(body, b"")


class TestProtocol(unittest.TestCase):
    def test_discover(self):
        _, frame = call("server/discover")
        result = result_of(frame)
        self.assertIn("2026-07-28", result["supportedVersions"])

    def test_tools_list(self):
        _, frame = call("tools/list")
        names = [t["name"] for t in result_of(frame)["tools"]]
        self.assertEqual(names, ["agent_run", "agent_sessions"])
        for t in result_of(frame)["tools"]:
            self.assertIn("description", t)
            self.assertEqual(t["inputSchema"]["type"], "object")

    def test_unknown_method(self):
        _, frame = call("no/such/method")
        self.assertEqual(frame["error"]["code"], -32601)
        self.assertIn("no/such/method", frame["error"]["message"])

    def test_unknown_tool(self):
        _, frame = call("tools/call", {"name": "nope", "arguments": {}})
        self.assertEqual(frame["error"]["code"], -32602)
        self.assertIn("unknown tool", frame["error"]["message"])

    def test_agent_run_bad_arguments(self):
        for args in ["not json", {"text": ""}]:
            _, frame = call("tools/call", {"name": "agent_run", "arguments": args})
            self.assertEqual(frame["error"]["code"], -32602, args)

    def test_session_id_header(self):
        headers, _ = call("tools/list")
        self.assertEqual(headers.get("Mcp-Session-Id"), "agent-mcp-demo")


class TestErrorPathNoCreds(unittest.TestCase):
    """无凭据:agent_run 不炸协议,以 isError=true 的文本结果回填。"""

    @classmethod
    def setUpClass(cls):
        cls.proc = spawn(env={"LLM_API_KEY": "", "LLM_APIKEY": ""},
                         cwd=tempfile.mkdtemp(prefix="agent-mcp-nocreds-"),
                         port=PORT + 1)

    @classmethod
    def tearDownClass(cls):
        stop(cls.proc)

    def test_run_without_creds_is_tool_error(self):
        _, frame = call("tools/call", port=PORT + 1,
                        params={"name": "agent_run", "arguments": {"text": "ping"}})
        result = result_of(frame)
        self.assertTrue(result["isError"])
        self.assertIn("no API key", result["content"][0]["text"])

    def test_sessions_without_creds_is_tool_error(self):
        _, frame = call("tools/call", port=PORT + 1,
                        params={"name": "agent_sessions", "arguments": {}})
        self.assertTrue(result_of(frame)["isError"])


class TestLiveCall(unittest.TestCase):
    """真实 LLM 调用:仅当 .env 提供凭据时运行。"""

    @classmethod
    def setUpClass(cls):
        env = load_dotenv(os.path.join(ROOT, ".env"))
        cls.creds = {
            k: env.get(k) or os.environ.get(k, "")
            for k in ("LLM_API_KEY", "LLM_APIKEY", "LLM_BASE_URL", "LLM_MODEL")
        }
        if not (cls.creds["LLM_BASE_URL"] and cls.creds["LLM_MODEL"]
                and (cls.creds["LLM_API_KEY"] or cls.creds["LLM_APIKEY"])):
            raise unittest.SkipTest("no LLM credentials; skipping live call")
        cls.proc = spawn(env=cls.creds)

    @classmethod
    def tearDownClass(cls):
        stop(cls.proc)

    def rpc_tool(self, name, arguments, timeout=120):
        req = urllib.request.Request(
            URL, data=json.dumps({
                "jsonrpc": "2.0", "id": 1,
                "method": "tools/call",
                "params": {"name": name, "arguments": arguments},
            }).encode(), method="POST",
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
        frame = json.loads(body[len(b"event: message\ndata: "):-2])
        self.assertIsNone(frame.get("error"), frame)
        return frame["result"]

    def test_run_and_multi_turn_session(self):
        result = self.rpc_tool("agent_run", {
            "text": "只回复一个词:PONG", "session": "blackbox"})
        self.assertFalse(result["isError"], result)
        self.assertIn("PONG", result["content"][0]["text"])

        result = self.rpc_tool("agent_run", {
            "text": "我上一条消息让你回复的词是什么?只回复那个词",
            "session": "blackbox"})
        self.assertFalse(result["isError"], result)
        self.assertIn("PONG", result["content"][0]["text"])

    def test_sessions_listing(self):
        result = self.rpc_tool("agent_sessions", {})
        self.assertFalse(result["isError"], result)


# ---- mDNS 发现(端到端:发现 → 交互) ----

MDNS_PORT = 5353
MDNS_GROUP = "224.0.0.251"


def dns_name_bytes(name):
    out = b""
    for label in name.rstrip(".").split("."):
        out += bytes([len(label)]) + label.encode()
    return out + b"\x00"


def mdns_query_ptr(service="_agent-mcp._tcp.local"):
    hdr = b"\x12\x34" + b"\x00\x00" + b"\x00\x01" + b"\x00\x00" * 3  # ID,flags,QD,AN,NS,AR
    return hdr + dns_name_bytes(service) + b"\x00\x0c\x00\x01"  # PTR, IN


def parse_mdns_response(data, want_suffix="_agent-mcp._tcp"):
    """极简解析:遍历 answer/additional,抓 PTR/SRV/A/TXT。"""
    def read_name(off):
        labels, jumped, end = [], False, None
        while True:
            b = data[off]
            if b == 0:
                off += 1
                break
            if b & 0xC0 == 0xC0:
                if end is None:
                    end = off + 2
                off = (b & 0x3F) << 8 | data[off + 1]
                jumped = True
                continue
            labels.append(data[off + 1:off + 1 + b].decode())
            off += 1 + b
        name = ".".join(labels)
        return name, end if jumped else off

    qd = int.from_bytes(data[4:6], "big")
    an = int.from_bytes(data[6:8], "big")
    ns = int.from_bytes(data[8:10], "big")
    ar = int.from_bytes(data[10:12], "big")
    off = 12
    for _ in range(qd):
        _, off = read_name(off)
        off += 4
    found = {"ptr": [], "srv": {}, "a": {}, "txt": {}}
    for _ in range(an + ns + ar):
        name, off = read_name(off)
        typ = int.from_bytes(data[off:off + 2], "big")
        rdlen = int.from_bytes(data[off + 8:off + 10], "big")
        rdata = data[off + 10:off + 10 + rdlen]
        roff = off
        off += 10 + rdlen
        if typ == 12:  # PTR: rdata 是(可能压缩的)域名,从绝对偏移解析
            pname, _ = read_name(roff + 10)
            if pname.endswith(want_suffix + ".local"):
                found["ptr"].append(pname)
        elif typ == 33:  # SRV: prio(2) weight(2) port(2) target
            port = int.from_bytes(rdata[4:6], "big")
            target, _ = read_name(roff + 10 + 6)
            found["srv"][name] = (port, target)
        elif typ == 1:  # A
            found["a"][name] = ".".join(str(b) for b in rdata)
        elif typ == 16:  # TXT
            kvs, i = {}, 0
            while i < len(rdata):
                l = rdata[i]
                if l == 0:
                    break
                k, _, v = rdata[i + 1:i + 1 + l].decode().partition("=")
                kvs[k] = v
                i += 1 + l
            found["txt"][name] = kvs
    return found


class TestMDNSDiscovery(unittest.TestCase):
    """启动参数 -mdns 开启广播;Python 直接发 mDNS 查询发现,再 HTTP 交互。"""

    NAME = "bb-agent"

    @classmethod
    def setUpClass(cls):
        env = load_dotenv(os.path.join(ROOT, ".env"))
        cls.creds = {
            k: env.get(k) or os.environ.get(k, "")
            for k in ("LLM_API_KEY", "LLM_APIKEY", "LLM_BASE_URL", "LLM_MODEL")
        }
        if not (cls.creds["LLM_BASE_URL"] and cls.creds["LLM_MODEL"]
                and (cls.creds["LLM_API_KEY"] or cls.creds["LLM_APIKEY"])):
            raise unittest.SkipTest("no LLM credentials; skipping mdns live discovery")
        cls.proc = spawn(env=cls.creds, port=PORT + 2,
                         extra_args=["-mdns", "-name", cls.NAME])

    @classmethod
    def tearDownClass(cls):
        stop(cls.proc)

    def test_discover_then_interact(self):
        # 1) mDNS 发现:发 PTR 查询,收单播响应。
        # 组播接口钉到 loopback:系统组播路由可能走隧道(如 wg),
        # lo 是本机同主机回环的可靠路径;应答器已在所有接口加入组播组。
        sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        sock.settimeout(5)
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF,
                        socket.inet_aton("127.0.0.1"))
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_LOOP, 1)
        sock.sendto(mdns_query_ptr(), (MDNS_GROUP, MDNS_PORT))
        hits = None
        deadline = time.time() + 8
        while time.time() < deadline:
            try:
                data, _ = sock.recvfrom(9000)
            except socket.timeout:
                continue
            found = parse_mdns_response(data)
            ptrs = [p for p in found["ptr"] if p.startswith(self.NAME + ".")]
            if not ptrs:
                continue
            inst = ptrs[0].split(".")[0]
            port, host = found["srv"].get(ptrs[0], (None, ""))
            ip = found["a"].get(host)
            txt = found["txt"].get(ptrs[0], {})
            hits = (inst, ip, port, txt)
            break
        sock.close()
        self.assertIsNotNone(hits, "mDNS: no answer within 8s")
        inst, ip, port, txt = hits
        self.assertEqual(inst, self.NAME)
        self.assertTrue(ip, "missing A record")
        self.assertIsInstance(port, int)
        self.assertEqual(txt.get("path"), "/mcp")

        # 2) 交互:用发现到的地址走 MCP 工具调用
        url = f"http://{ip}:{port}/mcp"
        req = urllib.request.Request(url, data=json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "tools/list",
        }).encode(), method="POST")
        with urllib.request.urlopen(req, timeout=10) as resp:
            body = resp.read()
        frame = json.loads(body[len(b"event: message\ndata: "):-2])
        names = [t["name"] for t in frame["result"]["tools"]]
        self.assertEqual(names, ["agent_run", "agent_sessions"])


# ---- 基础设施:编译并启动/停止被测服务器 ----

def wait_port(port=PORT, timeout=30.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"server did not listen on port {port} within {timeout}s")


def spawn(env, cwd=ROOT, port=PORT, extra_args=None):
    binpath = os.path.join(tempfile.mkdtemp(prefix="agent-mcp-"), "agent-mcp")
    subprocess.run(["go", "build", "-o", binpath, "./examples/agent-mcp"],
                   cwd=ROOT, check=True)
    child_env = {k: v for k, v in os.environ.items()
                 if not k.startswith("LLM_")}
    child_env.update(env)
    args = [binpath, "-addr", f"127.0.0.1:{port}"] + (extra_args or [])
    err = open("/tmp/agent-mcp-blackbox.log", "a")
    proc = subprocess.Popen(args,
                            cwd=cwd, env=child_env,
                            stdout=err, stderr=subprocess.STDOUT)
    try:
        wait_port(port)
    except Exception:
        proc.kill()
        raise
    return proc


def stop(proc):
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()


if __name__ == "__main__":
    unittest.main(verbosity=2)
