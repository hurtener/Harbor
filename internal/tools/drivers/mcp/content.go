package mcp

import (
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
	MIMEType string
	Data     []byte
}

// AudioRef is the lowered form of an MCP AudioContent.
type AudioRef struct {
	MIMEType string
	Data     []byte
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
	StructuredContent any
	Text              string
	Parts             []ContentPart
}

// ContentPart is the discriminated union of non-text content
// shapes. Exactly one of Image / Audio / Link / Embedded is set;
// Kind names which.
type ContentPart struct {
	Image    *ImageRef
	Audio    *AudioRef
	Link     *LinkRef
	Embedded *EmbeddedRef
	Kind     ContentKind
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
