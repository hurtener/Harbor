package agentcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// UnmarshalJSON rejects unsupported authority-bearing descriptor fields.
// In particular, reading a newer persisted pair or signed envelope must not
// silently discard a policy that this runtime cannot enforce.
func (d *SignedOAuthMCPConnectionDescriptor) UnmarshalJSON(raw []byte) error {
	type descriptor SignedOAuthMCPConnectionDescriptor
	var decoded descriptor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode signed MCP connection: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decode signed MCP connection: trailing JSON")
	}
	*d = SignedOAuthMCPConnectionDescriptor(decoded)
	return nil
}
