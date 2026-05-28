package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP content types (mcpsdk.Content) lower into one of these Harbor-
// facing shapes. The discrimination preserves the typed structure
// MCP returns ("brief 03 §4: MCP returns TextContent | ImageContent
// | EmbeddedResource | ResourceLink"); the LLM-edge enforcement
// pass (Phase 33) will route ≥-heavy-output-threshold byte payloads
// through the artifact store.
//
// Concurrent reuse (D-025): these are value types; no mutable state
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
}

// MarshalJSON renders the value LLM-edge-friendly. The text-only
// degenerate case (Parts + StructuredContent both empty) emits just
// the raw Text — most MCP tools return their result as a TextContent
// block carrying JSON-as-string, and the default struct marshal
// produces a `{"Text": "<escaped JSON>"}` wrapper that doubles the
// encoding and confuses LLMs (the Phase 107c step 10/11+follow-up
// live test pinned this: the YouTube agent looped re-emitting the
// same `youtube_get_metadata` because Claude Haiku 4.5 couldn't
// reliably parse the doubly-encoded wrapper). When Text is itself
// well-formed JSON, MarshalJSON emits the JSON value directly so the
// LLM reads a clean structure; otherwise the text rides as a JSON
// string. Audit / observability consumers that need the typed shape
// can re-derive it from the underlying CallToolResult on the bus.
//
// When StructuredContent is set, it wins (it's the MCP-server-typed
// projection). When Parts are non-empty, the wrapper carries the
// non-text shape verbatim — there is no clean unwrap for mixed-
// content responses, so the default struct render applies.
func (v MCPToolValue) MarshalJSON() ([]byte, error) {
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
// Concurrent reuse (D-025): pure function; no shared state.
func lowerCallToolResult(res *mcpsdk.CallToolResult) (MCPToolValue, error) {
	if res == nil {
		return MCPToolValue{}, nil
	}
	value := MCPToolValue{
		StructuredContent: res.StructuredContent,
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
		return value, fmt.Errorf("%w: %s", ErrMCPToolError, value.Text)
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
