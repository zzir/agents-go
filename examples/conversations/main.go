// Command conversations shows persisting history server-side with the OpenAI
// Conversations API via openai.ConversationsSession — no local store. The same
// session carries context across separate Run calls.
package main

import (
	"context"
	"fmt"
	"log"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// History lives under a server-side conversation ID, created lazily.
	sess := openai.NewConversationsSession()

	agent := &agents.Agent{Name: "assistant", Model: "gpt-4o"}
	opts := agents.RunOptions{Session: sess, ModelProvider: provider}

	if _, err := agents.Run(ctx, agent, "My name is Ada. Remember it.", opts); err != nil {
		log.Fatal(err)
	}

	res, err := agents.Run(ctx, agent, "What is my name?", opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString()) // should recall "Ada"

	id, _ := sess.ConversationID(ctx)
	fmt.Println("conversation id:", id)
}
