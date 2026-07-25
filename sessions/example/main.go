// Command example persists an agent's conversation in SQLite via the sessions
// module, so a follow-up question sees the previous turn.
//
// Run from the sessions module directory:
//
//	cd sessions && OPENAI_API_KEY=... go run ./example
//
// For PostgreSQL, open your own *sql.DB and use sessions.NewPostgres instead.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sessions"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	// A pure-Go (no CGO) SQLite file under the OS temp dir.
	dbPath := filepath.Join(os.TempDir(), "agents-session-demo.db")
	sess, db, err := sessions.NewSQLite("file:"+dbPath, "user-123")
	if err != nil {
		return err
	}
	defer db.Close()
	if err := sessions.CreateSchema(ctx, db); err != nil {
		return err
	}

	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("You are concise."),
		Model:        "gpt-4o",
	}
	opts := agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: openai.NewProvider()}}

	res1, err := agents.RunSync(ctx, agent, "What city is the Golden Gate Bridge in?", opts)
	if err != nil {
		return err
	}
	fmt.Println("Q1:", res1.FinalOutputString())

	// No history threading: the session replays turn 1 from SQLite.
	res2, err := agents.RunSync(ctx, agent, "What state is it in?", opts)
	if err != nil {
		return err
	}
	fmt.Println("Q2:", res2.FinalOutputString())

	fmt.Printf("\nConversation persisted to %s\n", dbPath)
	return nil
}
