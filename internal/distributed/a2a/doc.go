// Package a2a provides hand-transcribed Go shapes for every type
// defined in the vendored A2A v1 proto specification at
// `docs/specifications/a2a.proto` (commit
// `ae6a562d5d972f2c4b184f748bb32e1fa9aa7bf2`, 2026-04-23).
//
// The types are 1:1 with the proto messages, enums and oneofs:
//
//   - Every `message` in the proto has a Go struct counterpart.
//   - Every `enum` is represented as a typed int constant set with
//     `XxxUnspecified = 0` matching the proto's `_UNSPECIFIED = 0` rule.
//   - Every `oneof` is represented as a Go interface plus one concrete
//     type per variant; a `Kind() string` method discriminates at
//     runtime. The interface is sealed (unexported method) so external
//     packages cannot accidentally add variants.
//   - Every type's godoc quotes the source proto comment so the
//     transcription is auditable from inside the repo.
//
// Phase 22 does NOT depend on `google.golang.org/protobuf` or
// `google.golang.org/grpc`. The Go shapes JSON-marshal cleanly via
// stdlib `encoding/json`. Phase 29 (A2A southbound) may use generated
// gRPC code for the wire binding; the resulting types will be wrapped
// to match this package or translated at the boundary.
//
// JSON wire format notes (per the A2A spec + proto3 JSON mapping):
//
//   - `Part.raw` (`bytes`) is base64-encoded on the wire. `RawPart`
//     unmarshals from a base64 string.
//   - Discriminated unions (`Part`, `SecurityScheme`, `OAuthFlows`,
//     `StreamResponse`, `SendMessageResponse`) marshal as a flat object
//     with the variant's fields inline; `UnmarshalJSON` probes the
//     payload to pick the variant.
//   - Timestamps use RFC 3339; durations are not used.
//
// The package has no exported helpers that mutate state. It is a pure
// data-shape package.
package a2a
