# agent CLI 用法

以下是 `agent -h` 的完整 flag 一览(自动生成,勿手改):

```
Usage of agent:
  -C string
    	workspace root: run everything under this directory (applied before flag defaults; see pre-scan)
  -addr string
    	serve MCP Streamable HTTP on this address (e.g. 0.0.0.0:8788) instead of REPL; tools: agent_run(text, session?), agent_sessions()
  -agents-md string
    	AGENTS.md source: auto (discover from cwd up), off, or a file path (hook) (default "auto")
  -compress-keep int
    	recent messages to keep after compress (default 6)
  -compress-max-tokens int
    	session compress max tokens (e.g. 100000) (default 100000)
  -compress-ratio float
    	session compress trigger ratio 0-1 (e.g. 0.8 means 80%) (default 0.8)
  -confirm-tool
    	ask before every tool call (hook)
  -daemon
    	start the MCP server (-addr, default 127.0.0.1:8788) in the background and exit; logs to .agent/agent-mcp.log
  -daemon-stop
    	stop the background daemon started with -daemon
  -display-thinking
    	print thinking blocks as [thinking] (set to false to hide) (default true)
  -display-toolcall
    	print tool calls as [edit]/[read] etc. (set to false to hide) (default true)
  -fs string
    	builtin file tools read/write/edit; off/none disables (main) (default "on")
  -max-tokens int
    	max_tokens per LLM turn (default 1024)
  -max-turns int
    	max think-act-observe turns per question (<=0 = default 60) (default 60)
  -mcp value
    	MCP stdio server, repeatable: <name>=<command> [args...]
  -mdns
    	announce this agent via mDNS (_agent-mcp._tcp) for discovery (requires -addr or -daemon)
  -model string
    	model override (default: LLM_MODEL or NAME_MODEL)
  -name string
    	mDNS instance name (requires -mdns) (default "agent-mcp")
  -pprof string
    	serve net/http/pprof on <addr> (e.g. localhost:6060); on = localhost:6060; empty/off/none disables (hook)
  -provider string
    	env prefix: NAME reads NAME_API_KEY/NAME_APIKEY, NAME_BASE_URL, NAME_MODEL
  -q string
    	one-shot question (default: interactive REPL; with piped stdin, reads the question from stdin)
  -reasoning-effort string
    	reasoning effort passed through, e.g. low/medium/high; empty = endpoint default (main)
  -session string
    	persistent session name: resume if exists, autosave each turn (main)
  -session-compress string
    	session compress: auto/on/off (auto triggers at threshold) (default "auto")
  -sessions
    	list saved sessions and exit (main)
  -shell string
    	builtin shell tool; off/none disables (main) (default "on")
  -shell-escape string
    	REPL "!" shell escape; off/none disables (hook) (default "on")
  -skills string
    	skills directory (relative to cwd or absolute); off/none disables (hook) (default ".agents/skills")
  -slashcmd string
    	REPL "/" commands, e.g. /help; off/none disables (hook) (default "on")
  -temperature float
    	sampling temperature; <0 = endpoint default (main) (default -1)
  -term-title string
    	write the latest user input to the terminal title (OSC 0 via /dev/tty); off/none disables (hook) (default "on")
  -timeout duration
    	per-call timeout for -addr/-daemon server mode (default 10m0s)
  -update
    	self-update: download latest release and replace this binary (main)
  -verbose
    	print LLM turns and tool outcomes to stderr (hook)
  -version
    	print version and exit (main)
```
