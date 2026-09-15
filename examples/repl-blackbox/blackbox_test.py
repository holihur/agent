#!/usr/bin/env python3
"""CLI REPL 黑盒测试:把 cmd/agent 当纯黑盒,经真实 pty 驱动交互式 REPL。

用法:

    python3 examples/repl-blackbox/blackbox_test.py

覆盖:
  - 真实二进制:测试脚本自行 `go build ./cmd/agent`,不注入任何包内标识
  - 真实终端:pty.openpty() 分配伪终端,使 stdin 是 TTY,从而进入 REPL(而非管道一次性)
  - 真实协议:内置假 LLM HTTP 服务器,分别验证
      LLM_API=openai → POST /v1/chat/completions(Chat Completions 老接口)
      默认(anthropic)  → POST /v1/messages(Messages API)
    以 SSE 流式回包,断言请求落点与流式增量拼接
  - 多轮:同一 REPL 会话内连问两次,验证历史累积与循环可恢复
  - /exit 优雅退出,退出码 0

仅用标准库(pty/subprocess/http.server/select...),不依赖第三方。
"""

import fcntl
import http.server
import json
import os
import pty
import select
import struct
import subprocess
import tempfile
import termios
import threading
import time
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
MODEL = "mock-model"


# ---- 假 LLM 服务器:按路径区分协议,SSE 回显最后一条 user 文本 ----

class _LLMHandler(http.server.BaseHTTPRequestHandler):
    paths = []
    lock = threading.Lock()

    def log_message(self, *args):  # 静默
        pass

    def _last_user(self, path, req):
        last = ""
        if path == "/v1/chat/completions":
            for m in req.get("messages", []):
                if m.get("role") == "user":
                    last = m.get("content") or ""
        else:  # /v1/messages:content 是块数组
            for m in req.get("messages", []):
                if m.get("role") == "user":
                    for b in m.get("content", []):
                        if b.get("type") == "text":
                            last = b.get("text") or ""
        return last

    def _sse(self, payload):
        self.wfile.write(("data: " + json.dumps(payload) + "\n\n").encode())
        self.wfile.flush()

    def do_POST(self):
        with _LLMHandler.lock:
            _LLMHandler.paths.append(self.path)
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) if length else b""
        req = json.loads(raw or b"{}")
        answer = "echo:" + self._last_user(self.path, req)

        self.send_response(200)
        if req.get("stream"):
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            if self.path == "/v1/chat/completions":
                self._sse({"choices": [{"delta": {"role": "assistant", "content": ""}, "finish_reason": None}]})
                self._sse({"choices": [{"delta": {"content": answer}, "finish_reason": None}]})
                self._sse({"choices": [{"delta": {}, "finish_reason": "stop"}]})
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
            else:
                self._sse({"type": "message_start", "message": {"usage": {"input_tokens": 1}}})
                self._sse({"type": "content_block_start", "index": 0,
                           "content_block": {"type": "text", "text": ""}})
                self._sse({"type": "content_block_delta", "index": 0,
                           "delta": {"type": "text_delta", "text": answer}})
                self._sse({"type": "content_block_stop", "index": 0})
                self._sse({"type": "message_delta", "delta": {"stop_reason": "end_turn"}})
                self._sse({"type": "message_stop"})
        else:
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            if self.path == "/v1/chat/completions":
                body = {"choices": [{"message": {"role": "assistant", "content": answer},
                                     "finish_reason": "stop"}]}
            else:
                body = {"type": "message", "role": "assistant",
                        "content": [{"type": "text", "text": answer}], "stop_reason": "end_turn"}
            self.wfile.write(json.dumps(body).encode())


def start_mock_llm():
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _LLMHandler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd


def mock_paths():
    with _LLMHandler.lock:
        return list(_LLMHandler.paths)


def reset_mock_paths():
    with _LLMHandler.lock:
        _LLMHandler.paths = []


# ---- pty 会话驱动 ----

class PtySession:
    """把子进程挂在伪终端上,读写 master 端。"""

    def __init__(self, proc, master):
        self.proc = proc
        self.master = master
        self.buf = b""
        self.eof = False

    def pump(self, timeout=0.1):
        r, _, _ = select.select([self.master], [], [], timeout)
        if not r:
            return
        try:
            data = os.read(self.master, 65536)
        except OSError:
            data = b""
        if data:
            self.buf += data
        else:
            self.eof = True

    def wait_for(self, needle, timeout=20.0):
        want = needle.encode()
        deadline = time.time() + timeout
        while time.time() < deadline:
            if want in self.buf:
                return True
            self.pump(0.05)
            if self.eof and self.proc.poll() is not None:
                break
        return want in self.buf

    def wait_for_after(self, needle, start, timeout=15.0):
        """等 needle 出现在 buf[start:] —— 用于等待 agent 收尾后 readline 重新
        打印提示符,避免在 ESC 监听仍活跃时输入导致首字节被吞。"""
        want = needle.encode()
        deadline = time.time() + timeout
        while time.time() < deadline:
            idx = self.buf.find(want, start)
            if idx >= 0:
                return idx
            self.pump(0.05)
        return -1

    def write(self, line):
        os.write(self.master, (line + "\n").encode())
        time.sleep(0.05)

    def wait_exit(self, timeout=15.0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc.poll() is not None:
                return self.proc.returncode
            self.pump(0.05)
        return None

    def text(self):
        return self.buf.decode("utf-8", "replace")

    def close(self):
        try:
            if self.proc.poll() is None:
                self.proc.kill()
                self.proc.wait(timeout=5)
        except Exception:
            pass
        finally:
            try:
                os.close(self.master)
            except OSError:
                pass


def build_agent():
    bindir = tempfile.mkdtemp(prefix="agent-repl-")
    binpath = os.path.join(bindir, "agent")
    subprocess.run(["go", "build", "-o", binpath, "./cmd/agent"],
                   cwd=ROOT, check=True)
    return binpath


def clean_env(**overrides):
    env = {k: v for k, v in os.environ.items() if not k.startswith("LLM_")}
    env.update(overrides)
    return env


def spawn_repl(binpath, env):
    master, slave = pty.openpty()
    # 足够大的窗口,避免答案被终端折行破坏子串匹配。
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 50, 200, 0, 0))
    proc = subprocess.Popen(
        [binpath, "-shell", "off", "-fs", "off"],
        stdin=slave, stdout=slave, stderr=slave,
        env=env, cwd=tempfile.mkdtemp(prefix="agent-repl-cwd-"),
        start_new_session=True,
    )
    os.close(slave)
    return PtySession(proc, master)


class TestCLIRepl(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.binpath = build_agent()
        cls.httpd = start_mock_llm()
        cls.base_url = f"http://127.0.0.1:{cls.httpd.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()

    def _repl(self, api):
        reset_mock_paths()
        env = clean_env(LLM_API_KEY="test-key", LLM_BASE_URL=self.base_url, LLM_MODEL=MODEL)
        if api:
            env["LLM_API"] = api
        s = spawn_repl(self.binpath, env)
        self.addCleanup(s.close)
        return s

    def _answer_prompt(self, s, answer):
        """等一次答案出现,再等其后的一次提示符(证明 monitor 已停、readline 就绪)。"""
        self.assertTrue(s.wait_for(answer), f"no answer {answer!r}:\n{s.text()}")
        end = s.buf.find(answer.encode()) + len(answer.encode())
        self.assertGreaterEqual(s.wait_for_after("> ", end), 0,
                                f"no prompt after {answer!r}:\n{s.text()}")

    def _drive(self, api, want_path):
        s = self._repl(api)

        # 1) 启动 banner(证明进入 REPL,而非管道一次性路径)+ 首个提示符
        self.assertTrue(s.wait_for(f"model: {MODEL}"), f"no banner:\n{s.text()}")
        self.assertTrue(s.wait_for("> "), f"no prompt:\n{s.text()}")

        # 2) 第一轮:问 → 流式答
        s.write("1+1 等于几?只答数字")
        self._answer_prompt(s, "echo:1+1 等于几?只答数字")

        # 3) 第二轮:REPL 循环可恢复 + 历史累积
        s.write("再答一次")
        self._answer_prompt(s, "echo:再答一次")

        # 4) /exit 优雅退出
        s.write("/exit")
        self.assertIsNotNone(s.wait_exit(), f"did not exit:\n{s.text()}")
        self.assertEqual(s.proc.returncode, 0, s.text())

        # 5) 协议落点断言
        paths = mock_paths()
        self.assertGreaterEqual(len(paths), 2, f"paths={paths}")
        for p in paths:
            self.assertEqual(p, want_path, f"paths={paths}")

    def test_openai_chat_completions(self):
        """LLM_API=openai 走 Chat Completions 老接口。"""
        self._drive("openai", "/v1/chat/completions")

    def test_anthropic_messages_default(self):
        """未设置 LLM_API 时默认走 Anthropic Messages。"""
        self._drive("", "/v1/messages")


if __name__ == "__main__":
    unittest.main(verbosity=2)
