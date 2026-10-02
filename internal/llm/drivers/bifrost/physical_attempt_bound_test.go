package bifrost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/network"
	providerutils "github.com/maximhq/bifrost/core/providers/utils"
	bfschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"

	"github.com/hurtener/Harbor/internal/llm"
)

// The upstream fully reads and records the POST before closing without response
// headers. A reused socket therefore says nothing about whether work happened.
// Three independently warmed sockets make the pinned transport replay the same
// logical request four times. Everything is local; no provider or payment exists.
func TestPinnedBifrostTransportPhysicalPOSTBound(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			physical := processedPOSTReplays(t, stream)
			d := &Driver{provider: bfschemas.OpenAI, account: &Account{provider: bfschemas.OpenAI, primaryConfig: &bfschemas.ProviderConfig{NetworkConfig: bfschemas.NetworkConfig{MaxRetries: 1}}}}
			declared, err := d.ProviderAttemptBound(t.Context(), llm.CompleteRequest{})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("one SDK transport request produced %d fully consumed POSTs; current allocation declaration=%d", physical, declared)
			if physical != 4 {
				t.Fatalf("pinned SDK replay behavior changed: got %d consumed POSTs, want 4", physical)
			}
			if int64(declared) < physical {
				t.Fatalf("hard allocation underbounds physical sends: declaration %d < observed %d", declared, physical)
			}
		})
	}
}

func processedPOSTReplays(t *testing.T, stream bool) int64 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]struct{}{}
	warmReady := make(chan struct{}, 3)
	warmRelease := make(chan struct{})
	var releaseOnce sync.Once
	acceptDone := make(chan struct{})
	var physical atomic.Int64
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = struct{}{}
			mu.Unlock()
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				defer func() { mu.Lock(); delete(connections, conn); mu.Unlock() }()
				reader := bufio.NewReader(conn)
				warmed := false
				for {
					_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
					r, err := http.ReadRequest(reader)
					if err != nil {
						return
					}
					raw, err := io.ReadAll(io.LimitReader(r.Body, 1024))
					_ = r.Body.Close()
					if err != nil {
						return
					}
					if r.Method != http.MethodPost {
						return
					}
					switch r.URL.Path {
					case "/warm":
						warmed = true
						warmReady <- struct{}{}
						<-warmRelease
						if _, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
							return
						}
					case "/inference":
						if string(raw) != "fixture-inference-body" {
							return
						}
						physical.Add(1) // The synthetic billable effect happens BEFORE the loss.
						if warmed {
							return
						} // Header EOF after a fully consumed POST on pooled socket.
						_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
						return
					default:
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		releaseOnce.Do(func() { close(warmRelease) })
		mu.Lock()
		for c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		handlers.Wait()
	})
	factory := network.NewHTTPClientFactory(nil, nil)
	base := factory.GetFasthttpClient(network.ClientPurposeInference)
	base.ReadTimeout, base.WriteTimeout = 5*time.Second, 5*time.Second
	base.MaxConnsPerHost = 3
	base = providerutils.ConfigureDialer(base, true)
	client := base
	if stream {
		client = providerutils.BuildStreamingClient(base)
	}
	defer client.CloseIdleConnections()
	call := func(path, body string) error {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://" + listener.Addr().String() + path)
		req.Header.SetMethod(http.MethodPost)
		req.SetBodyString(body)
		_, failure, wait := providerutils.MakeRequestWithContext(context.Background(), client, req, resp)
		defer wait()
		if failure != nil {
			return fmt.Errorf("local SDK call failed: %+v", failure)
		}
		if resp.StatusCode() != 200 || string(resp.Body()) != "ok" {
			return fmt.Errorf("unexpected local response %d", resp.StatusCode())
		}
		return nil
	}
	var warming sync.WaitGroup
	warmErrors := make(chan error, 3)
	for range 3 {
		warming.Add(1)
		go func() { defer warming.Done(); warmErrors <- call("/warm", "warm") }()
	}
	for range 3 {
		select {
		case <-warmReady:
		case <-time.After(5 * time.Second):
			releaseOnce.Do(func() { close(warmRelease) })
			warming.Wait()
			t.Fatal("three independent sockets were not warmed")
		}
	}
	releaseOnce.Do(func() { close(warmRelease) })
	warming.Wait()
	close(warmErrors)
	for err := range warmErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = call("/inference", "fixture-inference-body"); err != nil {
		t.Fatal(err)
	}
	return physical.Load()
}
