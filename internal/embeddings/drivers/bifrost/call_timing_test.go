package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/telemetry"
)

func TestEmbedTiming_UsageAndPrivacy(t *testing.T) {
	const privateInput = "private embedding input and credential query"
	for _, tc := range []struct {
		name  string
		usage *bfschemas.BifrostLLMUsage
	}{
		{"unknown", nil},
		{"reported", &bfschemas.BifrostLLMUsage{PromptTokens: 17}},
		{"reported zero", &bfschemas.BifrostLLMUsage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			d, _ := newStubDriver(func(req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
				response, err := okResponse(req)
				response.Usage = tc.usage
				return response, err
			})
			d.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			ctx := telemetry.BeginTurnTiming(identityCtx(t), d.logger, time.Now())
			if _, err := d.Embed(ctx, []string{privateInput}); err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["role"] != "embedding" || record["outcome"] != "ok" || record["model"] != "text-embedding-3-small" {
				t.Fatalf("record=%v", record)
			}
			_, known := record["input_tokens"]
			if known != (tc.usage != nil) {
				t.Errorf("input token presence=%v", known)
			}
			if tc.usage != nil && record["input_tokens"] != float64(tc.usage.PromptTokens) {
				t.Errorf("input tokens=%v", record["input_tokens"])
			}
			for _, key := range []string{"ttft_ms", "output_tokens", "reasoning_effort"} {
				if _, ok := record[key]; ok {
					t.Errorf("unexpected %s", key)
				}
			}
			if strings.Contains(logs.String(), privateInput) {
				t.Fatal("input leaked")
			}
		})
	}
}

func TestEmbedTiming_CancellationPreservesErrorIdentity(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			var logs bytes.Buffer
			base := identityCtx(t)
			ctx, cancel := context.WithCancel(base)
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(base, 5*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			d, _ := newStubDriver(func(*bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
				if deadline {
					<-ctx.Done()
				} else {
					cancel()
				}
				return nil, &bfschemas.BifrostError{Error: &bfschemas.ErrorField{Message: "private canceled provider diagnostic"}}
			})
			d.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			_, err := d.Embed(ctx, []string{"private input"})
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			outcome := "canceled"
			if deadline {
				outcome = "timeout"
			}
			if record["outcome"] != outcome {
				t.Fatalf("timing = %v", record)
			}
			if strings.Contains(logs.String(), "private") {
				t.Fatal("private provider material leaked")
			}
		})
	}
}

func TestEmbedTiming_PanicRecordsAndPropagates(t *testing.T) {
	var logs bytes.Buffer
	original := &struct{ message string }{"private provider panic"}
	d, _ := newStubDriver(func(*bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
		panic(original)
	})
	d.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	func() {
		defer func() {
			if got := recover(); got != original {
				t.Fatalf("panic = %v, want original", got)
			}
		}()
		_, _ = d.Embed(identityCtx(t), []string{"private input"})
	}()
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["outcome"] != "error" || record["total_ms"] == nil {
		t.Fatalf("record = %v", record)
	}
	if strings.Contains(logs.String(), "private") {
		t.Fatalf("panic payload leaked: %s", logs.String())
	}
}
