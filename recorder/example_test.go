package recorder_test

import (
	"context"
	"fmt"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
	"github.com/techbridgeinnovation/agentpulse/recorder"
)

// A service that is not an agent on a framework reports for itself.
//
// Two things happen once at startup, and one line happens at each place a model
// gets called. Nothing here states a cost: the service says what happened and
// the server prices it against the rates that applied at the time.
func Example() {
	// Once, where the service starts. In a real service the sink is
	// recorder.NewGRPCSink(conn, "organisations/techbridge") and the recorder
	// is closed on shutdown.
	rec := recorder.New(recorder.Config{
		Sinks:      []recorder.Sink{recorder.Discard()},
		FlushEvery: time.Millisecond,
	})

	reporter := rec.For(recorder.Attribution{
		Agent:    "organisations/techbridge/agents/sources",
		Service:  "sources-service",
		Provider: pb.Activity_VERTEX_AI,
	})

	// Once per request, where the request arrives and the sign-in has been
	// checked. Everything recorded under this context groups together, so cost
	// can be read per request rather than only per model call. The identifier
	// goes on every record; the name goes once to the directory, so a report
	// can say who spent what.
	ctx := recorder.WithUser(
		recorder.WithRequest(context.Background(), "requests/7f3a"),
		recorder.User{ID: "8c21e0b4", Name: "Ada Lovelace", Email: "ada@example.com"},
	)

	// At each model call. The counts come from what the provider reported, and
	// the component says which part of the service spent it.
	started := time.Now()
	usage := callTheModel()
	reporter.ModelCall(ctx, recorder.ModelCall{
		Model:     "gemini-2.5-pro",
		Component: "asset_summary",
		Duration:  time.Since(started),
		Tokens: recorder.Tokens{
			Prompt:    usage.prompt,
			Candidate: usage.candidate,
			Cached:    usage.cached,
			Reasoning: usage.reasoning,
		},
	})

	// A tool charged per use costs money that no token count describes, so it
	// is recorded too, naming the rate it is charged against.
	reporter.ToolCall(ctx, recorder.ToolCall{
		Tool:     "web_search",
		Duration: 240 * time.Millisecond,
		Charges: []recorder.Charge{{
			PriceableUnit: "priceableUnits/vertex-ai-grounded-search-request",
			Quantity:      1,
		}},
	})

	// When the request finishes, release its running total.
	reporter.FinishRequest("requests/7f3a")

	closing, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec.Close(closing)

	stats := rec.Stats()
	fmt.Printf("recorded %d, dropped %d, delivered %d\n", stats.Recorded, stats.Dropped, stats.Delivered)
	// Output: recorded 2, dropped 0, delivered 2
}

type usage struct {
	prompt, candidate, cached, reasoning int32
}

func callTheModel() usage {
	return usage{prompt: 4200, candidate: 180, cached: 1024, reasoning: 96}
}
