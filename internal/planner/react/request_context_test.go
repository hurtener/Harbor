package react_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
)

func TestReact_RequestRebuilderOnlyForActivePreparation(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"absent", "nil compactor", "negative target", "automatic target", "explicit target"} {
		t.Run(scenario, func(t *testing.T) {
			q := fixedQuadruple(t, scenario)
			ctx := ctxWith(t, q)
			prep := llm.ContextPreparation{Compact: func(context.Context, int, int) (bool, error) {
				t.Fatal("request construction must not invoke compaction")
				return false, nil
			}}
			switch scenario {
			case "nil compactor":
				prep.Compact = nil
			case "negative target":
				prep.InputTarget = -1
			case "explicit target":
				prep.InputTarget = 12000
			}
			if scenario != "absent" {
				ctx = llm.WithContextPreparation(ctx, prep)
			}
			client := &captureReqClient{resp: llm.CompleteResponse{Content: "done"}}
			events := &recordingEmit{}
			p := react.New(client)
			if _, err := p.Next(ctx, rcWith(q, "preserve the current request", events.emit)); err != nil {
				t.Fatal(err)
			}
			req := client.lastReq.Load()
			if req == nil {
				t.Fatal("no actual request captured")
			}
			want := scenario == "automatic target" || scenario == "explicit target"
			if (req.RebuildMessages != nil) != want {
				t.Fatalf("request rebuilder present = %v, want %v", req.RebuildMessages != nil, want)
			}
			if !want {
				return
			}
			before := len(events.snapshot())
			messages, err := req.RebuildMessages()
			if err != nil || !reflect.DeepEqual(messages, req.Messages) {
				t.Fatalf("rebuild changed unchanged messages: %v", err)
			}
			if len(events.snapshot()) != before {
				t.Fatal("rebuild emitted another planner event")
			}
		})
	}
}
