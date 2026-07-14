package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxSSELineBytes  = 1 << 20
	defaultMaxSSEFrameBytes = 4 << 20
)

var (
	// ErrMalformedSSE reports invalid framing or a non-JSON data payload.
	ErrMalformedSSE = errors.New("protocol client: malformed SSE frame")
	// ErrSSELineTooLarge reports an SSE field line beyond its safety bound.
	ErrSSELineTooLarge = errors.New("protocol client: SSE line too large")
	// ErrSSEFrameTooLarge reports an SSE frame beyond its safety bound.
	ErrSSEFrameTooLarge = errors.New("protocol client: SSE frame too large")
	// ErrStreamClosed reports Recv on an explicitly closed stream.
	ErrStreamClosed = errors.New("protocol client: event stream closed")
)

// StreamOptions selects one identity-scoped event stream. Reconnect policy is
// caller-owned; LastEventID is the cursor supplied for this connection.
type StreamOptions struct {
	RunID       string
	EventTypes  []string
	LastEventID string
	Admin       bool
}

// SSEFrame is one decoded Server-Sent Events frame. Data is retained as strict
// JSON so reducers can select their own canonical event payload projection.
type SSEFrame struct {
	Event   string
	ID      string
	Data    json.RawMessage
	Comment string
	Retry   time.Duration
}

type streamResult struct {
	frame SSEFrame
	err   error
}

// EventStream owns one HTTP response and reader goroutine. Close is idempotent,
// cancels the request, closes the body, and joins the reader.
type EventStream struct {
	cancel context.CancelFunc
	body   io.ReadCloser
	result chan streamResult
	done   chan struct{}
	once   sync.Once
}

// Subscribe opens the canonical identity-scoped SSE stream.
func (c *client) Subscribe(ctx context.Context, options StreamOptions) (*EventStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := c.newRequest(streamCtx, http.MethodGet, "/v1/events", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if options.RunID != "" {
		req.Header.Set("X-Harbor-Run", options.RunID)
	}
	for _, eventType := range options.EventTypes {
		if strings.TrimSpace(eventType) != "" {
			req.Header.Add("X-Harbor-Event-Type", eventType)
		}
	}
	if options.LastEventID != "" {
		req.Header.Set("Last-Event-ID", options.LastEventID)
	}
	if options.Admin {
		query := req.URL.Query()
		query.Set("admin", "1")
		req.URL.RawQuery = query.Encode()
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("protocol client: subscribe: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		cancel()
		return nil, c.decodeError(resp)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%w: Content-Type %q", ErrMalformedSSE, contentType)
	}
	stream := &EventStream{
		cancel: cancel,
		body:   resp.Body,
		result: make(chan streamResult, 16),
		done:   make(chan struct{}),
	}
	go stream.readLoop(streamCtx)
	return stream, nil
}

// Recv waits for the next frame. Cancelling this receive does not close the
// stream; cancelling the Subscribe context or calling Close does.
func (s *EventStream) Recv(ctx context.Context) (SSEFrame, error) {
	select {
	case <-ctx.Done():
		return SSEFrame{}, ctx.Err()
	case result, ok := <-s.result:
		if !ok {
			return SSEFrame{}, ErrStreamClosed
		}
		return result.frame, result.err
	}
}

// Close cancels and joins the stream reader. It is safe to call repeatedly.
func (s *EventStream) Close() error {
	var closeErr error
	s.once.Do(func() {
		s.cancel()
		closeErr = s.body.Close()
		<-s.done
	})
	return closeErr
}

func (s *EventStream) readLoop(ctx context.Context) {
	defer close(s.done)
	defer close(s.result)
	reader := &frameReader{reader: bufio.NewReaderSize(s.body, defaultMaxSSELineBytes+1), firstLine: true}
	for {
		frame, err := reader.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.send(ctx, streamResult{err: err})
			return
		}
		if !s.send(ctx, streamResult{frame: frame}) {
			return
		}
	}
}

func (s *EventStream) send(ctx context.Context, result streamResult) bool {
	select {
	case s.result <- result:
		return true
	case <-ctx.Done():
		return false
	}
}

type frameReader struct {
	reader      *bufio.Reader
	firstLine   bool
	lastEventID string
}

func readFrame(reader *bufio.Reader) (SSEFrame, error) {
	return (&frameReader{reader: reader, firstLine: true}).readFrame()
}

func (r *frameReader) readFrame() (SSEFrame, error) {
	var frame SSEFrame
	var data [][]byte
	frameBytes := 0
	hasField := false
	for {
		line, err := readLine(r.reader)
		if err != nil {
			if errors.Is(err, io.EOF) && hasField {
				return r.finishFrame(frame, data)
			}
			return SSEFrame{}, err
		}
		if r.firstLine {
			r.firstLine = false
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		frameBytes += len(line) + 1
		if frameBytes > defaultMaxSSEFrameBytes {
			return SSEFrame{}, ErrSSEFrameTooLarge
		}
		if len(line) == 0 {
			if !hasField {
				continue
			}
			return r.finishFrame(frame, data)
		}
		hasField = true
		if line[0] == ':' {
			comment := strings.TrimSpace(string(line[1:]))
			if frame.Comment == "" {
				frame.Comment = comment
			} else {
				frame.Comment += "\n" + comment
			}
			continue
		}
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			frame.Event = string(value)
		case "id":
			if bytes.IndexByte(value, 0) < 0 {
				r.lastEventID = string(value)
			}
		case "data":
			data = append(data, bytes.Clone(value))
		case "retry":
			milliseconds, parseErr := strconv.ParseUint(string(value), 10, 64)
			const maxRetryMilliseconds = uint64((1<<63 - 1) / int64(time.Millisecond))
			if parseErr == nil && milliseconds <= maxRetryMilliseconds {
				frame.Retry = time.Duration(milliseconds) * time.Millisecond
			}
		}
	}
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 128)
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		switch value {
		case '\n':
			return line, nil
		case '\r':
			if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == '\n' {
				if _, readErr := reader.ReadByte(); readErr != nil {
					return nil, readErr
				}
			}
			return line, nil
		default:
			line = append(line, value)
			if len(line) > defaultMaxSSELineBytes {
				return nil, ErrSSELineTooLarge
			}
		}
	}
}

func (r *frameReader) finishFrame(frame SSEFrame, data [][]byte) (SSEFrame, error) {
	frame.ID = r.lastEventID
	if len(data) == 0 {
		return frame, nil
	}
	joined := bytes.Join(data, []byte{'\n'})
	var value any
	if err := decodeStrict(joined, &value); err != nil {
		return SSEFrame{}, fmt.Errorf("%w: data is not one JSON value: %w", ErrMalformedSSE, err)
	}
	frame.Data = json.RawMessage(bytes.Clone(joined))
	return frame, nil
}
