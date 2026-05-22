# Recipe: define an in-process tool

Register a plain Go function as a Harbor tool. The in-process driver
derives JSON Schemas from your typed input/output structs by
reflection and wraps the function in the `ToolPolicy` reliability
shell (timeout + retry + validation) — a plain registration is
production-resilient by construction.

The full runnable version of this recipe is
[`examples/tools/weather/`](../../examples/tools/weather/).

## Steps

1. **Declare typed input and output structs.** JSON Schema is derived
   from the `json` struct tags:

   ```go
   type LookupArgs struct {
       City string `json:"city"`
   }

   type LookupResult struct {
       City    string  `json:"city"`
       TempC   float64 `json:"temp_c"`
       Summary string  `json:"summary"`
   }
   ```

2. **Write the tool body.** It MUST be safe to invoke concurrently
   (CLAUDE.md §5, D-025) — hold no mutable state, read only from the
   arguments and `ctx`. Honour context cancellation:

   ```go
   func lookup(ctx context.Context, in LookupArgs) (LookupResult, error) {
       if err := ctx.Err(); err != nil {
           return LookupResult{}, fmt.Errorf("weather.lookup: %w", err)
       }
       if in.City == "" {
           return LookupResult{}, fmt.Errorf("weather.lookup: city is required")
       }
       return LookupResult{City: in.City, TempC: 21.0, Summary: "clear skies"}, nil
   }
   ```

3. **Register it against a catalog** with `inproc.RegisterFunc`:

   ```go
   import (
       "github.com/hurtener/Harbor/internal/tools"
       "github.com/hurtener/Harbor/internal/tools/drivers/inproc"
   )

   cat := tools.NewCatalog()
   err := inproc.RegisterFunc[LookupArgs, LookupResult](
       cat,
       "weather.lookup",
       lookup,
       tools.WithDescription("Look up current weather conditions by city name."),
       tools.WithTags("example", "weather"),
       tools.WithSideEffect(tools.SideEffectExternal),
   )
   ```

   Common `DescriptorOption`s (from `internal/tools/policy.go`):
   `WithDescription`, `WithTags`, `WithAuthScopes`, `WithSideEffect`,
   `WithExamples`, `WithCostHint`, `WithLatencyHint`, `WithPolicy`.

4. **Resolve and invoke** (this is what a planner does internally):

   ```go
   desc, ok := cat.Resolve("weather.lookup")
   // desc.Validate(args) runs the derived schema validator;
   // desc.Invoke(ctx, args) runs the policy-wrapped function.
   ```

## Notes

- `RegisterFunc` fails loudly on a duplicate name or an
  unrepresentable type — never silently. Check the returned error.
- Heavy outputs (≥ the configured `heavy_output_threshold_bytes`)
  route through the ArtifactStore automatically (D-022, D-026); your
  tool returns typed values, not blobs.
- HTTP, MCP, and A2A tools use different drivers
  (`internal/tools/drivers/{http,mcp,a2a}`) but the same catalog
  surface — see the `tools` block in
  [`examples/harbor.yaml`](../../examples/harbor.yaml).
