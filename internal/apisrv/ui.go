package apisrv

// indexHTML 是内置的单页聊天 UI：无构建、无外部依赖，直接消费 /api/chat 的
// SSE 流。它只是协议的一个参考实现，宿主（如 gitdash）可用自己的 UI 替换。
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>agent</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { margin: 0; font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         display: flex; flex-direction: column; height: 100vh; background: #0b0d12; color: #e6e6e6; }
  header { padding: 10px 16px; border-bottom: 1px solid #232733; display: flex; align-items: center; gap: 10px; }
  header b { font-weight: 600; }
  header .sess { margin-left: auto; font: 12px ui-monospace, monospace; color: #8b93a7; }
  #log { flex: 1; overflow-y: auto; padding: 18px 16px; display: flex; flex-direction: column; gap: 12px; }
  .msg { max-width: 820px; white-space: pre-wrap; word-break: break-word; }
  .msg.user { align-self: flex-end; background: #1d4ed8; color: #fff; padding: 8px 12px; border-radius: 12px 12px 2px 12px; }
  .msg.assistant { align-self: flex-start; }
  .msg.error { align-self: flex-start; color: #f87171; }
  .tool { align-self: flex-start; font: 12px/1.45 ui-monospace, monospace; color: #8b93a7;
          border-left: 2px solid #2a3040; padding: 2px 0 2px 10px; max-width: 820px; white-space: pre-wrap; }
  .tool.err { color: #f87171; border-color: #7f1d1d; }
  form { display: flex; gap: 8px; padding: 12px 16px; border-top: 1px solid #232733; }
  textarea { flex: 1; resize: none; min-height: 46px; max-height: 180px; padding: 10px 12px;
             border-radius: 10px; border: 1px solid #2a3040; background: #11141b; color: inherit; font: inherit; }
  button { padding: 0 18px; border: 0; border-radius: 10px; background: #2563eb; color: #fff; font: inherit; cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
  button.stop { background: #7f1d1d; }
</style>
</head>
<body>
<header><b>agent</b><span class="muted" id="status">idle</span><span class="sess" id="sess"></span></header>
<div id="log"></div>
<form id="form">
  <textarea id="input" placeholder="Ask the agent to inspect or change the repository… (Enter to send, Shift+Enter for newline)"></textarea>
  <button id="send" type="submit">Send</button>
</form>
<script>
const log = document.getElementById('log');
const form = document.getElementById('form');
const input = document.getElementById('input');
const sendBtn = document.getElementById('send');
const statusEl = document.getElementById('status');
const sessEl = document.getElementById('sess');
const params = new URLSearchParams(location.search);
const session = params.get('session') || 'default';
sessEl.textContent = 'session: ' + session;
let running = false;
let curAssistant = null;
let curDelta = '';

function add(cls, text) {
  const el = document.createElement('div');
  el.className = 'msg ' + cls;
  el.textContent = text;
  log.appendChild(el);
  log.scrollTop = log.scrollHeight;
  return el;
}
function addTool(name, text, isErr) {
  const el = document.createElement('div');
  el.className = 'tool' + (isErr ? ' err' : '');
  el.textContent = '⚙ ' + name + (text ? '\n' + text : '');
  log.appendChild(el);
  log.scrollTop = log.scrollHeight;
  return el;
}
function setRunning(v) {
  running = v;
  statusEl.textContent = v ? 'running…' : 'idle';
  sendBtn.textContent = v ? 'Stop' : 'Send';
  sendBtn.classList.toggle('stop', v);
  sendBtn.disabled = false;
  input.disabled = v;
}

async function cancel() {
  await fetch('/api/cancel', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session }) }).catch(() => {});
}

async function send(text) {
  add('user', text);
  curAssistant = null;
  curDelta = '';
  setRunning(true);
  let res;
  try {
    res = await fetch('/api/chat', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session, text }) });
  } catch (e) { add('error', 'network: ' + e); setRunning(false); return; }
  if (!res.ok || !res.body) { add('error', 'HTTP ' + res.status + ' ' + await res.text().catch(() => '')); setRunning(false); return; }

  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const chunk = buf.slice(0, idx); buf = buf.slice(idx + 2);
      const line = chunk.split('\n').find(l => l.startsWith('data: '));
      if (!line) continue;
      let ev; try { ev = JSON.parse(line.slice(6)); } catch { continue; }
      handle(ev);
    }
  }
  setRunning(false);
}

function handle(ev) {
  if (ev.type === 'delta') {
    if (!curAssistant) { curAssistant = add('assistant', ''); }
    curDelta += ev.text;
    curAssistant.textContent = curDelta;
    log.scrollTop = log.scrollHeight;
  } else if (ev.type === 'tool_start') {
    addTool(ev.name, ev.input || '', false);
  } else if (ev.type === 'tool_end') {
    addTool(ev.name, ev.result || '', !!ev.is_error);
  } else if (ev.type === 'done') {
    if (!curAssistant) { curAssistant = add('assistant', ev.text || ''); }
    else if (ev.text) { curAssistant.textContent = ev.text; }
    curAssistant = null;
  } else if (ev.type === 'error') {
    add('error', ev.error || 'error');
  }
}

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  if (running) { await cancel(); return; }
  const text = input.value.trim();
  if (!text) return;
  input.value = '';
  send(text);
});
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); form.requestSubmit(); }
});
</script>
</body>
</html>
`
