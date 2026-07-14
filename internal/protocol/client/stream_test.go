package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadFrame_FramingMultilineRetryCommentAndEOF(t *testing.T) {
	input := "retry: 125\nevent: example\nid: 42\ndata: {\"value\":\ndata: 1}\n: alive\n\n"
	frame, err := readFrame(bufio.NewReader(strings.NewReader(input)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if frame.Event != "example" || frame.ID != "42" || frame.Retry != 125*time.Millisecond {
		t.Fatalf("frame metadata = %+v", frame)
	}
	if string(frame.Data) != "{\"value\":\n1}" || frame.Comment != "alive" {
		t.Fatalf("frame data/comment = %+v", frame)
	}
	_, err = readFrame(bufio.NewReader(strings.NewReader("")))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty error = %v", err)
	}
}

func TestReadFrame_WHATWGBOMLineEndingsIgnoredFieldsAndEOFTail(t *testing.T) {
	input := "\xef\xbb\xbfid: first\rdata: {\"n\":1}\r\r" +
		"id: bad\x00id\r\nretry: later\r\ndata: {\"n\":2}\n\n" +
		"retry: 25\rdata: {\"n\":3}"
	reader := &frameReader{
		reader:    bufio.NewReaderSize(strings.NewReader(input), defaultMaxSSELineBytes+1),
		firstLine: true,
	}
	first, err := reader.readFrame()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if first.ID != "first" || string(first.Data) != `{"n":1}` {
		t.Fatalf("first frame = %+v", first)
	}
	second, err := reader.readFrame()
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if second.ID != "first" || second.Retry != 0 || string(second.Data) != `{"n":2}` {
		t.Fatalf("ignored NUL id/invalid retry frame = %+v", second)
	}
	third, err := reader.readFrame()
	if err != nil {
		t.Fatalf("EOF-tail frame: %v", err)
	}
	if third.ID != "first" || third.Retry != 25*time.Millisecond || string(third.Data) != `{"n":3}` {
		t.Fatalf("EOF-tail frame = %+v", third)
	}
	if _, err := reader.readFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestReadFrame_RejectsMalformedAndOversizedSSE(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{name: "bad JSON", body: "data: {\n\n", want: ErrMalformedSSE},
		{name: "line limit", body: "data: " + strings.Repeat("x", defaultMaxSSELineBytes+1) + "\n\n", want: ErrSSELineTooLarge},
		{name: "frame limit", body: strings.Repeat(": "+strings.Repeat("x", 64<<10)+"\n", 65) + "\n", want: ErrSSEFrameTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := readFrame(bufio.NewReaderSize(strings.NewReader(test.body), defaultMaxSSELineBytes+1))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestClient_Subscribe_AdminAndReceiveCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("admin") != "1" {
			t.Errorf("admin query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	stream, err := testClient(t, server).Subscribe(context.Background(), StreamOptions{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Recv(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Recv = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClient_Subscribe_SendsCursorFiltersAndPreservesOrdering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Last-Event-ID"); got != "40" {
			t.Errorf("Last-Event-ID = %q", got)
		}
		if got := r.Header.Values("X-Harbor-Event-Type"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
			t.Errorf("event types = %v", got)
		}
		if got := r.Header.Get("X-Harbor-Run"); got != "run" {
			t.Errorf("run = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 41; i <= 43; i++ {
			_, _ = fmt.Fprintf(w, "event: ordered\nid: %d\ndata: {\"sequence\":%d}\n\n", i, i)
			flusher.Flush()
		}
	}))
	defer server.Close()
	stream, err := testClient(t, server).Subscribe(context.Background(), StreamOptions{
		RunID: "run", EventTypes: []string{"one", "two"}, LastEventID: "40",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for i := 41; i <= 43; i++ {
		frame, err := stream.Recv(context.Background())
		if err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
		if frame.ID != strconv.Itoa(i) {
			t.Fatalf("frame %d ID = %q", i, frame.ID)
		}
	}
}

func TestClient_Subscribe_CancellationAndExplicitCloseJoin(t *testing.T) {
	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(disconnected)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := testClient(t, server).Subscribe(ctx, StreamOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	if err := stream.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe stream cancellation")
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Recv after Close = %v", err)
	}
}

func TestClient_Subscribe_DecodesPreStreamProtocolErrorAndContentType(t *testing.T) {
	t.Run("typed error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"auth_rejected","message":"expired"}`))
		}))
		defer server.Close()
		_, err := testClient(t, server).Subscribe(context.Background(), StreamOptions{})
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != "auth_rejected" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("wrong content type", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()
		_, err := testClient(t, server).Subscribe(context.Background(), StreamOptions{})
		if !errors.Is(err, ErrMalformedSSE) {
			t.Fatalf("error = %v", err)
		}
	})
}
