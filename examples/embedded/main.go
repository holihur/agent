// Command embedded 演示把 agent 嵌入宿主 Go 程序:零 CLI、零 flag,纯程序装配。
//
// 运行(凭据走 env 或宿主自行加载的 .env):
//
//	LLM_API_KEY=... LLM_BASE_URL=... LLM_MODEL=... go run ./examples/embedded
//
// 能力全部按需开启:进程内工具 Tool、MCP 服务器 MCP、内置 shell Shell,
// 流式增量经 OnTextDelta 回调输出。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	agent "github.com/holihur/agent"
)

func main() {
	ag, err := agent.New() // 凭据缺省走 env;也可 agent.New(agent.Config{...}) 覆盖
	if err != nil {
		log.Fatal(err)
	}
	defer ag.Close()

	err = ag.Tool("now", "Returns the current time in RFC3339.",
		map[string]any{"type": "object"},
		func(_ context.Context, _ json.RawMessage) (string, error) {
			return time.Now().Format(time.RFC3339), nil
		})
	if err != nil {
		log.Fatal(err)
	}

	// 可选能力,按需开启:
	//   _ = ag.Shell()
	//   _ = ag.MCP(agent.MCPSpec{Name: "echo", Command: []string{"/tmp/echo-mcp"}})

	ag.OnTextDelta(func(d agent.TextDelta) { fmt.Print(d.Text) })

	answer, err := ag.Run(context.Background(), "现在几点了?用 now 工具回答")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n--- final: %s\n", answer)
}
