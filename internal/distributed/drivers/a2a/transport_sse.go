package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/a2a"
)

// sseMaxLineBytes bounds the longest SSE line the parser will accept.
// SSE frames can carry arbitrary text inside `data:`; we cap at 16 MiB
// per line so a hostile peer can't allocate unbounded buffers.
const sseMaxLineBytes int = 16 << 20

// openSSEStream issues a POST against endpoint with body (the JSON-RPC
// envelope built by the caller — the same envelope shape used for
// unary calls) and returns a RemoteEventStream wrapping the response.
// The caller is responsible for closing the returned stream (which
// closes the underlying response body + cancels the per-stream ctx).
//
// SSE conventions per the spec:
//   - Lines starting with `data:` carry the payload (JSON-encoded
//     a2a.StreamResponse).
//   - Lines starting with `:` are comments (ignored).
//   - Lines starting with `event:` carry an optional event-type label
//     (we treat them as advisory — the StreamResponse discriminator
//     is the load-bearing dispatch).
//   - A blank line terminates the current event.
func openSSEStream(ctx context.Context, httpc *http.Client, endpoint, method string, params any) (distributed.RemoteEventStream, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	body, err := buildSSERequestBody(method, params)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("a2a: build sse request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "harbor-a2a/1.0")
	applyIdentityHeader(ctx, req)

	resp, err := httpc.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("a2a: sse transport: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Drain & close to release the connection promptly.
		bod, _ := io.ReadAll(io.LimitReader(resp.Body, 256)) //nolint:errcheck // best-effort drain of an error-response body for a diagnostic snippet; a read error just yields a shorter snippet.
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("a2a: sse open: status=%d body=%q", resp.StatusCode, snippet(bod, 256))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
		// Tolerant: some proxies strip the content-type. Don't error
		// on a missing/empty header; only fail when an explicit
		// non-SSE type is present.
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("a2a: sse open: unexpected Content-Type %q", ct)
	}
	return newSSEStream(streamCtx, cancel, resp.Body), nil
}

// buildSSERequestBody encodes a JSON-RPC envelope around the SSE
// request. Shares the envelope shape with jsonRPCClient.Call so
// servers see one consistent request format.
func buildSSERequestBody(method string, params any) ([]byte, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("a2a: marshal sse params for %q: %w", method, err)
		}
		raw = b
	}
	envelope := jsonRPCRequest{
		JSONRPC: jsonRPCVersion,
		ID:      0, // SSE streams don't correlate replies by ID; the
		// peer fans events from a single open connection.
		Method: method,
		Params: raw,
	}
	return json.Marshal(envelope)
}

// sseStream wraps an HTTP response body and parses SSE events.
// Each Recv consumes the next complete event (`data:` block ending in
// a blank line) and parses it into an a2a.StreamResponse.
//
// Termination signals:
//   - The HTTP response body closes (peer finished) → Recv returns io.EOF.
//   - ctx.Done() fires → Recv returns ctx.Err().
//   - Close() was called → Recv returns io.EOF.
//
// Concurrent reuse (D-025): the stream wraps per-call state (scanner
// buffers + cancel func) and is NOT designed for concurrent Recv from
// multiple goroutines — A2A streams are single-consumer by spec. The
// per-stream goroutine that pumps the bufio.Scanner runs at most once
// per stream; closes via cancel() + body.Close().
type sseStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	br     *bufio.Reader

	closed atomic.Bool
	// readMu serialises Recv calls so a stray concurrent consumer
	// (test bug, mostly) sees a clean error instead of corrupting the
	// scanner buffer.
	readMu sync.Mutex
	// frameCount tracks how many events were parsed; used in
	// malformed-frame error messages.
	frameCount atomic.Uint64
}

// newSSEStream wraps body. The supplied ctx is cancelled when Close is
// invoked.
func newSSEStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser) *sseStream {
	return &sseStream{
		ctx:    ctx,
		cancel: cancel,
		body:   body,
		br:     bufio.NewReaderSize(body, 64<<10),
	}
}

// Recv parses and returns the next StreamResponse. Honours ctx.Done().
func (s *sseStream) Recv(ctx context.Context) (a2a.StreamResponse, error) {
	if s.closed.Load() {
		return a2a.StreamResponse{}, io.EOF
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()

	if err := s.ctx.Err(); err != nil {
		return a2a.StreamResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return a2a.StreamResponse{}, err
	}

	// Each event is a sequence of `data:` lines terminated by a
	// blank line. We accumulate the data lines into one JSON
	// payload, then unmarshal into a2a.StreamResponse.
	for {
		// Make sure we honour both caller's ctx and the stream's
		// own ctx (cancellation via Close()).
		if err := ctx.Err(); err != nil {
			return a2a.StreamResponse{}, err
		}

		dataLines, terminated, err := s.readEvent(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return a2a.StreamResponse{}, io.EOF
			}
			return a2a.StreamResponse{}, err
		}
		if !terminated && len(dataLines) == 0 {
			// EOF reached with no buffered event.
			return a2a.StreamResponse{}, io.EOF
		}
		if len(dataLines) == 0 {
			// Blank-line-only event (heartbeat / keepalive). Skip.
			continue
		}
		payload := strings.Join(dataLines, "\n")
		var sr a2a.StreamResponse
		if err := json.Unmarshal([]byte(payload), &sr); err != nil {
			n := s.frameCount.Add(1)
			return a2a.StreamResponse{}, fmt.Errorf("%w: frame=%d: %w", ErrSSEStreamMalformed, n, err)
		}
		if err := sr.Validate(); err != nil {
			n := s.frameCount.Add(1)
			return a2a.StreamResponse{}, fmt.Errorf("%w: frame=%d: %w", ErrSSEStreamMalformed, n, err)
		}
		s.frameCount.Add(1)
		return sr, nil
	}
}

// readEvent reads lines until a blank line ends the event or EOF
// terminates the stream. Returns the data lines collected (event:
// labels and comments are skipped).
func (s *sseStream) readEvent(ctx context.Context) ([]string, bool, error) {
	var data []string
	// Inject a goroutine-free cancel hook: every iteration checks
	// ctx.Err() so a Close()-triggered cancel surfaces within one
	// line-read of the bufio reader. Worst-case latency is one
	// line-of-bytes from the peer.
	for {
		if err := ctx.Err(); err != nil {
			return data, false, err
		}
		line, err := readBoundedLine(s.br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if line == "" {
					return data, true, io.EOF
				}
				// Append the final partial line if it carried data
				// and a missing newline.
				if d, ok := dataLine(line); ok {
					data = append(data, d)
				}
				return data, true, io.EOF
			}
			return data, false, fmt.Errorf("a2a: sse read: %w", err)
		}
		// Strip trailing CR/LF.
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// Blank line → event terminator.
			return data, true, nil
		}
		if strings.HasPrefix(line, ":") {
			// Comment line; ignore.
			continue
		}
		if d, ok := dataLine(line); ok {
			data = append(data, d)
		}
		// event: / id: / retry: labels are advisory; we don't
		// inspect them — the StreamResponse discriminator is the
		// load-bearing field.
	}
}

// readBoundedLine reads a single SSE line (terminated by '\n') from br,
// enforcing the sseMaxLineBytes cap so a hostile peer streaming an
// unterminated line cannot force unbounded buffer growth. The returned
// line includes any trailing newline (the caller strips CR/LF). On a
// genuine io.EOF the partial bytes read so far are returned alongside
// io.EOF, matching bufio.Reader.ReadString semantics. When the cap is
// exceeded, ErrSSELineTooLong is returned with whatever was buffered.
func readBoundedLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		b, err := br.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		sb.WriteByte(b)
		if b == '\n' {
			return sb.String(), nil
		}
		if sb.Len() > sseMaxLineBytes {
			return sb.String(), fmt.Errorf("%w: %d bytes without a newline (cap %d)",
				ErrSSELineTooLong, sb.Len(), sseMaxLineBytes)
		}
	}
}

// dataLine returns the value portion of an SSE `data:` line. Returns
// `false` when the line is not a `data:` line.
func dataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	v := strings.TrimPrefix(line, "data:")
	// Per the SSE spec, an optional leading space is stripped.
	v = strings.TrimPrefix(v, " ")
	return v, true
}

// Close releases the stream. Idempotent.
func (s *sseStream) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.cancel()
	return s.body.Close()
}
