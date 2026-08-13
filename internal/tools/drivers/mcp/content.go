package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
)

// MCP content types (mcpsdk.Content) lower into one of these Harbor-
// facing shapes. The discrimination preserves the typed structure
// MCP returns ("MCP returns TextContent | ImageContent
// | EmbeddedResource | ResourceLink"); the LLM-edge enforcement
// pass will route ≥-heavy-output-threshold byte payloads
// through the artifact store.
//
// Concurrent reuse: these are value types; no mutable state
// after construction.

// ImageRef is the lowered form of an MCP ImageContent.
type ImageRef struct {
	Data     []byte
	MIMEType string
}

// AudioRef is the lowered form of an MCP AudioContent.
type AudioRef struct {
	Data     []byte
	MIMEType string
}

// LinkRef is the lowered form of an MCP ResourceLink (a pointer to
// a resource the server hosts; the client may follow it with
// ReadResource if desired).
type LinkRef struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// EmbeddedRef is the lowered form of an MCP EmbeddedResource (a
// resource embedded directly in the tool call result).
type EmbeddedRef struct {
	URI      string
	MIMEType string
	Text     string
	Blob     []byte
}

// MCPToolValue is the typed shape returned from `Invoke` when the
// remote MCP server returns a CallToolResult. Heterogeneous parts
// preserve the wire ordering so downstream consumers (LLM context
// assembly, audit) can reconstruct the server's response.
type MCPToolValue struct {
	// Text concatenates every TextContent block in encounter order.
	Text string
	// Parts is the ordered, typed slice of every non-text content
	// block. Empty when the response is pure text.
	Parts []ContentPart
	// StructuredContent is the MCP `structuredContent` field on
	// servers that support typed JSON output (mcpsdk.ToolHandlerFor).
	// nil when absent.
	StructuredContent any
	// AppRef is the MCP Apps reference for an invoked tool that declared
	// an interactive UI. The canonical `io.modelcontextprotocol/ui`
	// (ext-apps) dialect binds the `ui://` UI resource to the tool
	// DEFINITION (`_meta.ui.resourceUri` on the tool, captured at
	// discovery); a server MAY additionally place a per-result hint on the
	// CallToolResult `_meta.ui` slot. The value here is the reconciled
	// reference — the tool-definition binding, merged with any per-result
	// hint — set on the result by `Provider.callTool`. `lowerCallToolResult`
	// alone populates it only from the per-result `_meta`; the binding is
	// merged in by `callTool`, which holds the discovery-time binding. It is
	// nil for ordinary (non-app) tools. AppRef is excluded from the JSON
	// wire form (`json:"-"`) so it never reaches the LLM-facing observation —
	// it is a host-side projection consumed by the app-tool-call proxy and
	// the discovery event, not planner context.
	AppRef *AppRef `json:"-"`
	// ArtifactEgress records the artifact values the runtime resolved
	// into THIS call's outbound arguments — one content-free entry per
	// substituted parameter (artifact id, parameter name, byte count,
	// `sha256:` digest). Nil for a tool that maps no artifact parameters.
	//
	// Unlike AppRef above, it is deliberately INCLUDED in the JSON wire
	// form and therefore reaches the observation and the trajectory. The
	// contrast is the design: AppRef is a host-side projection the model
	// has no business reading, whereas the model AUTHORED the artifact
	// id, and telling it "the id you named was delivered, N bytes" is
	// honest, content-free and replayable. Without it a model could not
	// distinguish a delivered document from an ignored parameter.
	//
	// It carries no bytes, so it is safe everywhere the observation goes.
	ArtifactEgress []artifactegress.Record `json:"artifact_egress,omitempty"`
}

// AppRef is the host-side reference to an MCP App — the interactive
// HTML UI an MCP tool declares via the official
// `io.modelcontextprotocol/ui` (ext-apps) extension. The canonical
// dialect binds the UI resource to the tool DEFINITION: the `ui://`
// resource URI rides the tool's `_meta.ui.resourceUri` slot, captured at
// discovery. It is recognised distinctly from ordinary content: ONLY a
// `ui://`-scheme URI is treated as an app. An ordinary `file://` /
// `https://` resource reference is never promoted to an AppRef.
//
// Concurrent reuse: AppRef is a value type with no mutable state after
// construction.
type AppRef struct {
	// ResourceURI is the `ui://`-scheme URI of the app's UI document.
	// The host fetches the document via a resource read scoped to the
	// request identity triple. Its canonical source is the tool
	// definition's `_meta.ui.resourceUri`.
	ResourceURI string
	// PreferredDisplayMode is an optional display-mode hint (one of
	// inline / fullscreen / pip), or empty when none was stated. The tool
	// definition's `_meta.ui` carries no display mode in the canonical
	// dialect, so this is empty on the golden path; a server MAY supply a
	// per-result hint on the CallToolResult `_meta.ui` slot, which the
	// host merges over the binding. When empty, the renderer defaults to
	// inline. It is a hint only; the host reconciles it against the
	// server's negotiated capability set.
	PreferredDisplayMode string
	// ToolCallID is the stable, per-invocation identifier minted at the
	// tool-call site (a content hash of the run / server / tool / args).
	// It correlates a discovered app to the captured tool context — the
	// input + lowered result that produced it — so a Protocol client can
	// fetch that context via mcp.apps.tool_context. It is NOT parsed from
	// the server's `_meta` (a result never carries it).
	//
	// A non-empty value is a PROMISE that a context record was persisted:
	// the invocation path stamps it only after the capture succeeded, so it
	// stays empty when no capturer is wired or the capture failed. A reader
	// may therefore treat a fetch miss as "the record is gone" rather than
	// "it may never have existed".
	ToolCallID string
}

// uiResourceScheme is the URI scheme the MCP Apps extension reserves
// for an app's UI document. ONLY a resource reference carrying this
// scheme is recognised as an app; ordinary file:// / https:// resources
// are left as plain content.
const uiResourceScheme = "ui://"

// IsUIResourceURI reports whether uri carries the reserved `ui://`
// scheme — the distinct recognition the MCP Apps extension requires so
// an ordinary file:// / https:// resource is never mistaken for an app.
func IsUIResourceURI(uri string) bool {
	return strings.HasPrefix(uri, uiResourceScheme)
}

// parseAppRef extracts the MCP Apps reference from an MCP `_meta` map.
// The same slot shape appears on a tool DEFINITION (the canonical
// placement — `Tool._meta.ui.resourceUri`, read at discovery) and,
// optionally, on a tool RESULT (`CallToolResult._meta.ui`, a per-result
// hint), so parseAppRef serves both. The extension carries the reference
// under the `ui` key as `{ resourceUri: "ui://…", … }`. parseAppRef
// returns nil when the slot is absent, malformed, or carries a
// non-`ui://`-scheme URI — so a definition or result that references an
// ordinary file:// / https:// resource is not promoted to an app. A
// loud-but-non-fatal posture: a malformed slot yields nil rather than an
// error, because a present-but-broken `_meta` must not poison an
// otherwise-valid tool or result.
func parseAppRef(meta mcpsdk.Meta) *AppRef {
	if len(meta) == 0 {
		return nil
	}
	uiRaw, ok := meta["ui"]
	if !ok {
		return nil
	}
	uiMap, ok := uiRaw.(map[string]any)
	if !ok {
		return nil
	}
	uri, ok := uiMap["resourceUri"].(string)
	if !ok || uri == "" || !IsUIResourceURI(uri) {
		return nil
	}
	return &AppRef{
		ResourceURI:          uri,
		PreferredDisplayMode: uiDisplayModeHint(meta),
	}
}

// uiDisplayModeHint extracts an optional display-mode hint from an MCP
// `_meta.ui` slot, independently of whether the slot also carries a
// `resourceUri`. The canonical tool-definition binding carries NO display
// mode, but a server MAY place a per-result display-mode hint on the
// CallToolResult `_meta.ui` slot to steer THIS result's presentation. The
// ext-apps dialect names it `preferredFrame`; `displayMode` is accepted as
// an alias some servers emit. Returns "" when absent or malformed.
func uiDisplayModeHint(meta mcpsdk.Meta) string {
	if len(meta) == 0 {
		return ""
	}
	uiMap, ok := meta["ui"].(map[string]any)
	if !ok {
		return ""
	}
	if mode, ok := uiMap["preferredFrame"].(string); ok {
		return mode
	}
	if mode, ok := uiMap["displayMode"].(string); ok {
		return mode
	}
	return ""
}

// modelFacingVisibilityValues are the `_meta.ui.visibility` entries that
// make a tool a candidate for model/planner-facing surfaces. A tool whose
// visibility array contains `app` and NONE of these is app-only: a
// callback for its rendered App, not an operation for the model to select.
// Unknown values are deliberately treated as non-model-facing — when in
// doubt, the callback stays OUT of planner context (the conservative
// direction: hiding a model-facing tool from the planner is a visibility
// miss; exposing an app-only callback to the model is a least-privilege
// leak).
var modelFacingVisibilityValues = map[string]struct{}{
	"tool":    {},
	"model":   {},
	"planner": {},
	"agent":   {},
	"all":     {},
}

// appVisibilityOnly reports whether an MCP tool's `_meta.ui.visibility`
// array declares it APP-ONLY — a callback for the tool's rendered App, not
// an operation for the model to select. The canonical
// `io.modelcontextprotocol/ui` (ext-apps) dialect carries the visibility
// list on the tool DEFINITION's `_meta.ui` slot, alongside `resourceUri`.
//
// The rule is conjunctive: the array must contain `app` AND contain no
// model-facing entry. `["app"]` is app-only; `["app", "tool"]` (or
// `["app", "all"]`) is visible to both; `["tool"]` and an absent or empty
// `visibility` are ordinary model-facing tools (the pre-existing default).
// A malformed slot (wrong types) is treated as absent — a present-but-
// broken `_meta` must not poison an otherwise-valid tool.
//
// This classification is DISCOVERY metadata, never an authorization
// shortcut: an app-only tool is still invoked only under the identity /
// reach / OAuth / approval / current-state gates, exactly like an ordinary
// tool. It only decides WHICH catalog view the tool is published into.
func appVisibilityOnly(meta mcpsdk.Meta) bool {
	if len(meta) == 0 {
		return false
	}
	uiMap, ok := meta["ui"].(map[string]any)
	if !ok {
		return false
	}
	raw, ok := uiMap["visibility"]
	if !ok {
		return false
	}
	var entries []string
	switch v := raw.(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				entries = append(entries, s)
			}
		}
	case []string:
		entries = v
	default:
		return false
	}
	hasApp := false
	for _, e := range entries {
		if e == "app" {
			hasApp = true
			continue
		}
		if _, modelFacing := modelFacingVisibilityValues[e]; modelFacing {
			return false
		}
	}
	return hasApp
}

// reconcileAppRef combines the tool-DEFINITION app binding (the
// spec-conformant source of the `ui://` resource URI — the canonical
// `io.modelcontextprotocol/ui` dialect binds the UI resource to the tool,
// captured at discovery) with an optional per-result hint a server may
// also place on the CallToolResult `_meta.ui` slot. The tool binding is
// the source of the resource URI; a per-result display-mode hint, when
// present, wins over the binding's so a server can steer THIS result's
// presentation. Returns nil when neither source declares an app.
//
// Conformant servers leave the result `_meta` empty, so resultHint is nil
// and resultMode is "" on the golden path and the binding stands alone. A
// per-result `_meta.ui` slot carrying only a display mode (no resourceUri)
// yields a nil resultHint but a non-empty resultMode — so a server can steer
// THIS result's presentation without re-declaring the resource URI. A server
// that (non-conformantly) declares the full app only on the result still
// surfaces, via the resultHint fallback — the reconcile never REQUIRES the
// result `_meta`.
func reconcileAppRef(toolBinding, resultHint *AppRef, resultMode string) *AppRef {
	if toolBinding == nil {
		return resultHint
	}
	eff := &AppRef{
		ResourceURI:          toolBinding.ResourceURI,
		PreferredDisplayMode: toolBinding.PreferredDisplayMode,
	}
	if resultMode != "" {
		eff.PreferredDisplayMode = resultMode
	}
	return eff
}

// MarshalJSON renders the value LLM-edge-friendly. The text-only
// degenerate case (Parts + StructuredContent both empty) emits just
// the raw Text — most MCP tools return their result as a TextContent
// block carrying JSON-as-string, and the default struct marshal
// produces a `{"Text": "<escaped JSON>"}` wrapper that doubles the
// encoding. When Text is itself well-formed JSON, MarshalJSON emits
// the JSON value directly so the LLM reads a clean structure;
// otherwise the text rides as a JSON string. Audit / observability
// consumers that need the typed shape can re-derive it from the
// underlying CallToolResult on the bus.
//
// When StructuredContent is set, it wins (it's the MCP-server-typed
// projection). When Parts are non-empty, the wrapper carries the
// non-text shape verbatim — there is no clean unwrap for mixed-
// content responses, so the default struct render applies.
// When this call carried an egress substitution, the collapsed body is
// WRAPPED alongside the content-free substitution record, so the model
// that authored the artifact id is told the id was delivered and how
// many bytes it was. Without the wrapper the collapse below would drop
// the record for the commonest (text-only) result shape, and the model
// could not distinguish a delivered document from an ignored parameter.
// A call with NO substitution is unaffected — its rendering is
// byte-identical to what it was before egress existed.
func (v MCPToolValue) MarshalJSON() ([]byte, error) {
	if len(v.ArtifactEgress) > 0 {
		body, err := v.marshalResultBody()
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Result         json.RawMessage         `json:"result"`
			ArtifactEgress []artifactegress.Record `json:"artifact_egress"`
		}{Result: body, ArtifactEgress: v.ArtifactEgress})
	}
	return v.marshalResultBody()
}

// marshalResultBody renders the tool RESULT itself, with the
// LLM-edge-friendly collapses described on [MCPToolValue.MarshalJSON].
func (v MCPToolValue) marshalResultBody() ([]byte, error) {
	hasParts := len(v.Parts) > 0
	hasStructured := v.StructuredContent != nil

	// Mixed content with non-text parts (image / audio / link / embedded)
	// keeps the full wrapper — there is no clean unwrap. Use a type alias
	// to dodge recursive MarshalJSON.
	if hasParts {
		type alias MCPToolValue
		return json.Marshal(alias(v))
	}

	// Pure structured-content (or text+structured where the structured
	// projection is the canonical typed view — typical for MCP tools that
	// quote their JSON body in a TextContent block AND also surface it as
	// StructuredContent for typed clients). The structured projection is
	// the LLM-friendly shape; the duplicated Text is the brief-07-era
	// fallback for non-tool-calling readers and is dropped on the wire to
	// avoid the doubly-encoded {"Text":"<escaped JSON>"} shape that
	// confuses native-tool-calling models.
	if hasStructured {
		return json.Marshal(v.StructuredContent)
	}

	// Text-only response. Prefer emitting the Text as a parsed JSON value
	// when it is well-formed JSON; fall back to a JSON string render.
	if v.Text == "" {
		return []byte("null"), nil
	}
	trimmed := strings.TrimSpace(v.Text)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		var probe any
		if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
			return json.Marshal(probe)
		}
	}
	return json.Marshal(v.Text)
}

// ContentPart is the discriminated union of non-text content
// shapes. Exactly one of Image / Audio / Link / Embedded is set;
// Kind names which.
type ContentPart struct {
	Kind     ContentKind
	Image    *ImageRef
	Audio    *AudioRef
	Link     *LinkRef
	Embedded *EmbeddedRef
}

// ContentKind discriminates a ContentPart.
type ContentKind string

// The ContentKind values, one per MCP content-part shape.
const (
	ContentKindImage    ContentKind = "image"
	ContentKindAudio    ContentKind = "audio"
	ContentKindLink     ContentKind = "link"
	ContentKindEmbedded ContentKind = "embedded"
)

// lowerCallToolResult converts an mcpsdk.CallToolResult into a
// Harbor-shaped MCPToolValue plus the IsError signal lifted into a
// returned error. `IsError == true` is mapped to ErrMCPToolError
// wrapping the rendered text body, so the policy classifier sees a
// retryable transient error class by default (the operator can
// override via ToolPolicy.RetryOn).
//
// Concurrent reuse: pure function; no shared state.
func lowerCallToolResult(res *mcpsdk.CallToolResult) (MCPToolValue, error) {
	if res == nil {
		return MCPToolValue{}, nil
	}
	value := MCPToolValue{
		StructuredContent: res.StructuredContent,
		// Parse the OPTIONAL per-result MCP Apps `_meta.ui` hint. The
		// canonical dialect binds the `ui://` resource to the tool
		// DEFINITION (reconciled in by Provider.callTool, which holds the
		// discovery-time binding); a server MAY additionally place a hint
		// here, most usefully a per-result display mode. Only a
		// `ui://`-scheme URI is promoted; an ordinary file:// / https://
		// resource is left untouched. Conformant servers leave this empty.
		AppRef: parseAppRef(res.Meta),
	}
	var texts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		case *mcpsdk.ImageContent:
			value.Parts = append(value.Parts, ContentPart{
				Kind:  ContentKindImage,
				Image: &ImageRef{Data: append([]byte(nil), v.Data...), MIMEType: v.MIMEType},
			})
		case *mcpsdk.AudioContent:
			value.Parts = append(value.Parts, ContentPart{
				Kind:  ContentKindAudio,
				Audio: &AudioRef{Data: append([]byte(nil), v.Data...), MIMEType: v.MIMEType},
			})
		case *mcpsdk.ResourceLink:
			value.Parts = append(value.Parts, ContentPart{
				Kind: ContentKindLink,
				Link: &LinkRef{
					URI:         v.URI,
					Name:        v.Name,
					Title:       v.Title,
					Description: v.Description,
					MIMEType:    v.MIMEType,
				},
			})
		case *mcpsdk.EmbeddedResource:
			ref := &EmbeddedRef{}
			if v.Resource != nil {
				ref.URI = v.Resource.URI
				ref.MIMEType = v.Resource.MIMEType
				ref.Text = v.Resource.Text
				if len(v.Resource.Blob) > 0 {
					ref.Blob = append([]byte(nil), v.Resource.Blob...)
				}
			}
			value.Parts = append(value.Parts, ContentPart{
				Kind:     ContentKindEmbedded,
				Embedded: ref,
			})
		default:
			// Unknown content types lower to TextContent of the JSON
			// marshalling — preserves the data without losing it.
			// MCP forward-compat: new content kinds appear over time.
			if data, err := c.MarshalJSON(); err == nil {
				texts = append(texts, string(data))
			}
		}
	}
	value.Text = strings.Join(texts, "")
	if res.IsError {
		class, message, ok := tools.MCPResultErrorClassification(map[string]any(res.Meta), res.StructuredContent)
		if !ok {
			// Legacy MCP results have only IsError and human-readable content.
			// Preserve that content in the lowered value, but never classify it
			// by parsing its text.
			message = value.Text
		}
		err := tools.NewMCPToolResultErrorClassification(class, message, ok)
		var typed *tools.MCPToolResultError
		if errors.As(err, &typed) {
			typed.Result = tools.ToolResult{Value: value}
		}
		return value, err
	}
	return value, nil
}

// lowerReadResourceResult converts an mcpsdk.ReadResourceResult
// into a Harbor-shaped MCPToolValue. The Contents slice is
// preserved as EmbeddedRef parts so the caller can pick out blob /
// text / mime data.
func lowerReadResourceResult(res *mcpsdk.ReadResourceResult) MCPToolValue {
	if res == nil {
		return MCPToolValue{}
	}
	value := MCPToolValue{}
	for _, rc := range res.Contents {
		if rc == nil {
			continue
		}
		ref := &EmbeddedRef{
			URI:      rc.URI,
			MIMEType: rc.MIMEType,
			Text:     rc.Text,
		}
		if len(rc.Blob) > 0 {
			ref.Blob = append([]byte(nil), rc.Blob...)
		}
		value.Parts = append(value.Parts, ContentPart{
			Kind:     ContentKindEmbedded,
			Embedded: ref,
		})
		if rc.Text != "" {
			value.Text += rc.Text
		}
	}
	return value
}

// lowerGetPromptResult converts an mcpsdk.GetPromptResult into a
// Harbor-shaped MCPToolValue. Each prompt message renders into the
// Text field with role prefixes so downstream LLM context
// assembly can reconstruct turns deterministically.
func lowerGetPromptResult(res *mcpsdk.GetPromptResult) MCPToolValue {
	if res == nil {
		return MCPToolValue{}
	}
	value := MCPToolValue{}
	var b strings.Builder
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		_, _ = fmt.Fprintf(&b, "[%s] ", m.Role)
		if m.Content != nil {
			if tc, ok := m.Content.(*mcpsdk.TextContent); ok {
				b.WriteString(tc.Text)
			} else if data, err := m.Content.MarshalJSON(); err == nil {
				b.Write(data)
			}
		}
		b.WriteString("\n")
	}
	value.Text = b.String()
	return value
}
