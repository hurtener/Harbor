// Package artifactegress carries the MCP arm of Harbor's
// pass-by-reference tool-argument routing: an operator declares that a
// named parameter on a named remote tool carries artifact BYTES, the
// model supplies an artifact id, and the runtime resolves that id and
// places the resolved bytes into the outbound tool-call body.
//
// It is the sibling of the in-process arm
// (internal/tools/artifactref), and it exists because that arm
// structurally cannot serve a remote tool. [artifactref.Substitute]
// walks a Go TYPE tree, binding a resolved value onto a declared
// artifactref.Ref field; a remote tool has no Go type — its input
// schema is authored by the remote server and its arguments arrive as
// an untyped map decoded from the model's JSON. Two functions for two
// structurally different inputs is not two implementations of one
// thing, and neither can stand in for the other.
//
// # What moves, and what does not
//
// Bytes flow store -> outbound request body. Nothing dereferenceable
// leaves the runtime: no address is published, no grant is minted, no
// reusable handle exists. The substituted value is bounded to ONE
// request body on ONE dispatched call.
//
// The reachable artifact SET is unchanged from the in-process arm — the
// dispatching run's own (tenant, user, session), enforced by the SAME
// ctx-seated resolver, which answers not-found for anything else. What
// changes is the RECIPIENT: a remote server can now receive the bytes.
// That widening is governed by an operator's byte-eligibility
// declaration on the connection, and it is stated rather than claimed
// away — see the trust boundary note below.
//
// # The wire encoding is normative
//
// The substituted value is a Go []byte behind [Payload], written into
// the DECODED argument map at the mapped key and emitted on the wire as
// RFC 4648 §4 standard base64 with padding.
//
// It is NEVER a Go string. encoding/json rewrites every invalid-UTF-8
// byte in a Go string to U+FFFD, so a binary document placed in a
// string slot arrives corrupted and longer than it started — the exact
// defect the artifact read path was corrected for. A []byte behind a
// carrier round-trips byte-exact.
//
// It is not an MCP typed content block either, because no such thing
// exists on the argument side: CallToolParams.Arguments is an arbitrary
// JSON value validated against the server's own inputSchema, and the
// content union (text / image / audio / resource) appears only on
// results and sampling messages. The option was measured, not
// preferred.
//
// # The projection bound
//
// [Payload] keeps the bytes in an unexported field and projects itself
// through every serialisation door Go offers: [Payload.MarshalJSON]
// emits the base64 string (the one door that MUST carry content), while
// [Payload.String] and [Payload.LogValue] emit a reference. A Payload
// that reaches fmt or slog therefore emits a reference BY CONSTRUCTION,
// which is the same idiom artifactref.Ref uses and the reason the
// resolved value does not decay into a naked []byte once it leaves the
// encoder.
//
// [Encode] is the ONE content-emitting call site, and an AST scan
// (artifactref.ScanEgressSites) holds it to a reviewed list. That scan
// bounds where the encoder is CALLED, not where its output travels —
// stated honestly, because a call-site walk says nothing about data
// flow. The arrival side is covered by an integration suite that walks
// every sink the raw argument JSON reaches.
//
// # The trust boundary this package operates inside
//
// Byte-eligibility and the parameter mapping are admin-writable over
// the control plane. A tenant admin can therefore attach a server they
// control, map a parameter, declare the connection byte-eligible, and
// receive a user's artifact bytes on the next run that names an id.
//
// The claim this package makes is the SCOPED one: the reachable
// artifact set is the dispatching run's own triple and nothing wider.
// The claim it does NOT make is that the feature grants no reach to the
// admin who writes the field — measured at the admin, it does. That
// sits inside a trust boundary Harbor has already accepted and named: a
// shared runtime TRUSTS its co-tenant admins for runtime-added
// connections, and a deployment needing hard isolation runs one runtime
// per tenant. This package does not move that boundary and does not
// invent a new one.
//
// Artifact bytes are stored AS AUTHORED — unredacted — so an artifact
// may itself contain a credential. That is a fact about what can move,
// not an analogy, and it is why this package does not describe the
// eligibility field as "carrying no secret": that is true of the FIELD
// and irrelevant to the FLOW.
//
// The compensating control is the substitution [Record]: the FACT of
// every substitution — ids, sizes, a digest, never the bytes — is
// recorded before the wire request is issued. The one real difference
// between this path and an admin instructing a model to paste content
// into a tool argument is that pasting leaves a trace; the record
// restores it.
//
// # Concurrent reuse
//
// Nothing here holds mutable package-level state. A [Mapping] is
// immutable after [CompileMapping] and is captured by value into a
// driver's per-tool invocation closure; a [Payload] is created per
// invocation and lives on the per-call argument map.
package artifactegress

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// ErrEgressTooLarge is returned when a resolved artifact value exceeds
// the configured egress ceiling.
//
// It is LOUD rather than truncating. The artifact READ path truncates
// truthfully — a model asking for a window gets a window and is told it
// is one — but a truncated document delivered to a remote INGESTER is a
// corruption, not a bounded read: the server has no way to know it
// received half a file, and the truthful-truncation signal has nowhere
// to live in a tool argument.
var ErrEgressTooLarge = errors.New("artifactegress: resolved value exceeds the egress ceiling")

// ErrMappedArgumentMissing is returned when the arguments do not supply
// a required parameter the operator mapped as artifact-bearing.
//
// It is a refusal rather than a skip: the operator declared that this
// parameter carries artifact bytes, so an invocation that omits a
// required parameter is outside the declared contract. A genuinely
// optional parameter is marked with the trailing `?` mapping marker.
var ErrMappedArgumentMissing = errors.New("artifactegress: mapped artifact parameter was not supplied")

// ErrMappedArgumentNotString is returned when a mapped parameter is
// supplied as something other than a JSON string. The model authors an
// artifact ID and nothing else, so any other JSON shape is a malformed
// argument rather than a value to coerce.
var ErrMappedArgumentNotString = errors.New("artifactegress: mapped artifact parameter is not a string artifact id")

// ErrEmptyArtifactID is returned when a mapped parameter carries an
// empty artifact id. An omitted parameter and an explicitly empty one
// are different facts and both are refused, with different causes so an
// operator can tell them apart.
var ErrEmptyArtifactID = errors.New("artifactegress: mapped artifact parameter carries an empty artifact id")

// ErrInvalidMapping is returned by [CompileMapping] for a mapping that
// cannot be honoured: an empty tool name, an empty parameter name, or a
// parameter named twice for one tool. A parameter's trailing `?` is an
// optional marker and is not part of its wire name.
var ErrInvalidMapping = errors.New("artifactegress: invalid artifact parameter mapping")

// ErrInvalidCeiling is returned by [Encode] when it is handed a
// non-positive ceiling. The caller resolves the operator's configured
// value (which carries a documented default) before calling, so a
// non-positive one here is a wiring fault and is refused rather than
// silently treated as unbounded.
var ErrInvalidCeiling = errors.New("artifactegress: egress ceiling must be positive")

// Payload carries ONE resolved artifact value to ONE outbound tool
// call.
//
// The bytes live in an unexported field. Every serialisation door
// projects deliberately: [Payload.MarshalJSON] emits RFC 4648 §4
// standard base64 (the one door that must carry content), while
// [Payload.String] and [Payload.LogValue] emit a reference of the form
// `artifact <id> (<n> bytes)`. A Payload reaching fmt or slog therefore
// emits a reference by construction rather than by discipline.
//
// The zero Payload is a valid empty reference: its id is empty, its
// size is zero, and it marshals as an empty base64 string.
type Payload struct {
	id     string
	data   []byte
	digest string
}

// newPayload builds a Payload over the resolved bytes, computing the
// digest once at construction so the carrier is immutable afterwards.
func newPayload(id string, data []byte) Payload {
	sum := sha256.Sum256(data)
	return Payload{id: id, data: data, digest: "sha256:" + hex.EncodeToString(sum[:])}
}

// ID returns the artifact id the resolved value came from. It is the
// only part of a Payload that is safe to log, emit or persist.
func (p Payload) ID() string { return p.id }

// Size returns the byte length of the resolved content.
func (p Payload) Size() int { return len(p.data) }

// Digest returns the `sha256:<hex>` digest of the resolved content. It
// identifies WHICH bytes moved without carrying them, which is what
// makes the substitution record auditable.
func (p Payload) Digest() string { return p.digest }

// String renders the payload as a reference, so a `%v` or `%s` verb
// over a decoded argument map prints `artifact <id> (<n> bytes)` and
// never content.
func (p Payload) String() string {
	return fmt.Sprintf("artifact %s (%d bytes)", p.id, len(p.data))
}

// LogValue renders the payload as a reference, so `slog.Any("args",
// argMap)` over a decoded argument map logs `artifact <id> (<n>
// bytes)` and never content.
func (p Payload) LogValue() slog.Value { return slog.StringValue(p.String()) }

// MarshalJSON emits the resolved content as an RFC 4648 §4 standard
// base64 string with padding.
//
// This is the ONE door that carries content, and it is deliberate: the
// remote server declared a string-typed parameter, MCP has no
// argument-side typed content block, and a Go string slot would let
// encoding/json rewrite every invalid-UTF-8 byte to U+FFFD. Encoding
// from a []byte behind this carrier is what makes an arbitrary binary
// document arrive byte-exact.
func (p Payload) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(p.data))
}

var (
	_ json.Marshaler = Payload{}
	_ fmt.Stringer   = Payload{}
	_ slog.LogValuer = Payload{}
)

// Record is the FACT of one substitution: ids and sizes, never bytes.
//
// It rides the canonical event the driver emits before the wire request
// and the observation the planner sees afterwards. Both carry the same
// shape on purpose — an operator reading the audit trail and a model
// reading its own transcript are told the same thing, and neither is
// told the content.
type Record struct {
	// ArtifactID is the id the model authored and the runtime resolved.
	ArtifactID string `json:"artifact_id"`
	// Param is the remote tool's parameter name the bytes were written
	// into.
	Param string `json:"param"`
	// SizeBytes is the length of the resolved content.
	SizeBytes int `json:"size_bytes"`
	// Digest is the `sha256:<hex>` digest of the resolved content — which
	// bytes moved, without the bytes.
	Digest string `json:"digest"`
}

// Mapping is the compiled per-tool artifact-parameter mapping:
// which parameters on which remote tools carry artifact bytes.
//
// It is immutable after [CompileMapping] and is captured BY VALUE into
// a driver's per-tool invocation closure at discovery, so a live
// mapping change takes effect at the next attach or reconcile and never
// mid-flight. That is the next-turn projection posture every other
// connection field has, and it is what keeps the driver a compiled
// artifact with no mutable per-run state.
type Mapping struct {
	byTool map[string][]mappedParameter
}

// mappedParameter is the compiled form of one operator mapping entry. The
// optional marker is deliberately kept out of the exposed parameter list and
// out of the remote tool's schema name; it only changes missing-argument
// handling at Encode.
type mappedParameter struct {
	name     string
	optional bool
}

// CompileMapping validates and compiles an operator-declared mapping of
// remote tool name -> the parameter names on that tool which carry
// artifact bytes.
//
// The mapping is the OPERATOR's: a remote server never decides when the
// runtime reads its own store. A server-declared "this parameter is an
// artifact reference" annotation was considered and rejected — a remote
// server driving host-side privileged behaviour is exactly the
// host-obligation inversion the MCP host rules forbid.
//
// A parameter ending in `?` is optional: CompileMapping strips that marker
// and records the bare parameter name. Required parameters retain today's
// behavior. An empty or nil input compiles to the empty mapping, which
// matches no tool.
func CompileMapping(params map[string][]string) (Mapping, error) {
	if len(params) == 0 {
		return Mapping{}, nil
	}
	byTool := make(map[string][]mappedParameter, len(params))
	for tool, names := range params {
		trimmedTool := strings.TrimSpace(tool)
		if trimmedTool == "" {
			return Mapping{}, fmt.Errorf("%w: tool name must not be empty", ErrInvalidMapping)
		}
		if len(names) == 0 {
			return Mapping{}, fmt.Errorf("%w: tool %q maps no parameter names", ErrInvalidMapping, trimmedTool)
		}
		seen := make(map[string]struct{}, len(names))
		out := make([]mappedParameter, 0, len(names))
		for _, name := range names {
			trimmed := strings.TrimSpace(name)
			optional := strings.HasSuffix(trimmed, "?")
			if optional {
				trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "?"))
			}
			if trimmed == "" {
				return Mapping{}, fmt.Errorf("%w: tool %q maps an empty parameter name", ErrInvalidMapping, trimmedTool)
			}
			if _, dup := seen[trimmed]; dup {
				return Mapping{}, fmt.Errorf("%w: tool %q maps parameter %q twice", ErrInvalidMapping, trimmedTool, trimmed)
			}
			seen[trimmed] = struct{}{}
			out = append(out, mappedParameter{name: trimmed, optional: optional})
		}
		// Deterministic order so the substitution records, the emitted
		// event and the golden transcript do not depend on map iteration.
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		byTool[trimmedTool] = out
	}
	return Mapping{byTool: byTool}, nil
}

// ParamsFor returns the mapped parameter names for one remote tool, in
// deterministic order, or nil when the tool maps none. The returned
// slice is a copy: the compiled mapping is immutable and a caller
// cannot reach into it.
func (m Mapping) ParamsFor(tool string) []string {
	names, ok := m.byTool[tool]
	if !ok || len(names) == 0 {
		return nil
	}
	out := make([]string, len(names))
	for i, param := range names {
		out[i] = param.name
	}
	return out
}

// Tools returns every tool name the mapping addresses, sorted. It backs
// the attach-time check that every mapped tool exists in the server's
// discovered tool set.
func (m Mapping) Tools() []string {
	if len(m.byTool) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.byTool))
	for tool := range m.byTool {
		out = append(out, tool)
	}
	sort.Strings(out)
	return out
}

// IsEmpty reports whether the mapping addresses no tool at all. A
// connection with an empty mapping takes the untouched outbound path:
// its calls are byte-identical to a build without this feature.
func (m Mapping) IsEmpty() bool { return len(m.byTool) == 0 }

// Encode is THE one content-emitting call site.
//
// For each parameter the mapping declares on tool, it reads the
// artifact id from args, resolves it through the resolver seated on
// ctx, and writes the resulting [Payload] back into args IN PLACE at
// the same key. It returns one [Record] per substitution for the caller
// to emit and to stamp on the observation. A parameter compiled from a
// trailing `?` marker is optional: a missing key or nil value is skipped,
// while a present value follows the same validation and resolution path as
// a required parameter.
//
// args MUST be the DECODED argument map, never the raw argument JSON.
// The raw JSON is what the trajectory persists, what the observation
// renders, what the per-invocation content hash is computed over, and
// what the durable tool-context record replays into a browser; rewriting
// it would put the resolved value into every one of those sinks. Writing
// only into the decoded map keeps the substitution dispatch-local.
//
// It fails loudly rather than degrading, on all of:
//
//   - no resolver seated on ctx (artifactref.ErrNoResolver) — which is
//     also what a browser-driven MCP-App tool callback hits, because
//     that path has no run and therefore no seated resolver;
//   - a required mapped parameter the arguments did not supply
//     ([ErrMappedArgumentMissing]);
//   - a mapped parameter supplied as something other than a string
//     ([ErrMappedArgumentNotString]);
//   - an empty artifact id ([ErrEmptyArtifactID]);
//   - a resolver error, wrapped with the id that produced it — including
//     the not-found a cross-identity id produces;
//   - a resolved value above maxBytes ([ErrEgressTooLarge]), naming the
//     artifact, its size and the ceiling.
//
// Resolution happens ONCE per dispatched call, ahead of the reliability
// shell's retry loop. That is a correctness property as well as a
// memory one: an unresolvable id is a model mistake rather than a
// transient fault, so retrying it four times would burn the budget
// without changing the answer.
//
// Calls to this function are held to a reviewed list by
// artifactref.ScanEgressSites. Adding a second call site is a reviewed
// decision, not an edit.
func Encode(ctx context.Context, args map[string]any, m Mapping, tool string, maxBytes int) ([]Record, error) {
	params := m.byTool[tool]
	if len(params) == 0 {
		return nil, nil
	}
	var resolver artifactref.Resolver
	resolverSeated := false
	records := make([]Record, 0, len(params))
	for _, param := range params {
		raw, present := args[param.name]
		if !present || raw == nil {
			if param.optional {
				continue
			}
			return nil, fmt.Errorf("%w: tool %q parameter %q", ErrMappedArgumentMissing, tool, param.name)
		}
		if maxBytes <= 0 {
			return nil, fmt.Errorf("%w: tool %q got %d", ErrInvalidCeiling, tool, maxBytes)
		}
		id, isString := raw.(string)
		if !isString {
			return nil, fmt.Errorf("%w: tool %q parameter %q is %T", ErrMappedArgumentNotString, tool, param.name, raw)
		}
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: tool %q parameter %q", ErrEmptyArtifactID, tool, param.name)
		}
		if !resolverSeated {
			var ok bool
			resolver, ok = artifactref.ResolverFrom(ctx)
			if !ok {
				return nil, fmt.Errorf("%w: tool %q maps %d artifact parameter(s) but this invocation has no run-scoped resolver",
					artifactref.ErrNoResolver, tool, len(params))
			}
			resolverSeated = true
		}
		data, err := resolver.ResolveArtifact(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("artifactegress: resolve %q for tool %q parameter %q: %w", id, tool, param.name, err)
		}
		if len(data) > maxBytes {
			return nil, fmt.Errorf("%w: artifact %q for tool %q parameter %q is %d bytes, ceiling is %d — the value is refused rather than truncated, because a partial document delivered to a consumer is a corruption",
				ErrEgressTooLarge, id, tool, param.name, len(data), maxBytes)
		}
		payload := newPayload(id, data)
		args[param.name] = payload
		records = append(records, Record{
			ArtifactID: payload.ID(),
			Param:      param.name,
			SizeBytes:  payload.Size(),
			Digest:     payload.Digest(),
		})
	}
	if len(records) == 0 {
		return nil, nil
	}
	return records, nil
}
