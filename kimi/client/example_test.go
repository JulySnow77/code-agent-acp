package client_test

import (
	"context"
	"fmt"
	"os"

	"kimi-acp/acp"
	"kimi-acp/agent"
	"kimi-acp/client"
)

// ExampleClient shows the complete client-side flow against an in-process ACP
// agent. Replace the buffered pair with client.Start(...) to drive `kimi acp`.
func ExampleClient() {
	aTr, bTr := acp.NewBufferedPair()
	ag := agent.New(aTr, agent.NewEcho().Handlers(), agent.Options{})
	go func() { _ = ag.Conn().Run(context.Background()) }()

	c := client.Dial(bTr, client.Options{
		Handlers: client.HandlerFuncs{
			SessionUpdate: func(n *acp.SessionNotification) {
				if n.Update.Type() == acp.UpdateAgentMessageChunk {
					fmt.Print(n.Update.Text())
				}
			},
		},
	})
	defer func() { c.Close(); ag.Close() }()

	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	ns, err := c.NewSession(ctx, acp.NewSessionParams{CWD: os.TempDir()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	result, err := c.Prompt(ctx, ns.SessionID, []acp.ContentBlock{acp.TextBlock("hi")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println()
	fmt.Println(result.StopReason)
	// Output:
	// echo: hi
	// end_turn
}
