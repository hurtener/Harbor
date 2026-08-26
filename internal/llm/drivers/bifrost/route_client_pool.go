package bifrost

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	bf "github.com/maximhq/bifrost/core"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

const defaultRouteClientPoolCapacity = 32

var errRouteClientPoolBusy = errors.New("bifrost: provider route client pool is at capacity")

type routeClientPoolKey struct {
	TenantID                     string
	RuntimeID                    string
	RouteID                      string
	RouteGeneration              uint64
	ProviderConnectionID         string
	ProviderConnectionGeneration uint64
	CredentialAssetGeneration    uint64
	Provider                     string
	EndpointDigest               string
}

type routeClientPoolEntry struct {
	key    routeClientPoolKey
	client *bf.Bifrost
	refs   int
	elem   *list.Element
}

type routeClientPool struct {
	mu       sync.Mutex
	capacity int
	network  llm.NetworkDefaults
	entries  map[routeClientPoolKey]*routeClientPoolEntry
	lru      list.List
	closed   bool
}

func newRouteClientPool(capacity int, network llm.NetworkDefaults) *routeClientPool {
	if capacity <= 0 {
		capacity = defaultRouteClientPoolCapacity
	}
	return &routeClientPool{capacity: capacity, network: network, entries: make(map[routeClientPoolKey]*routeClientPoolEntry)}
}

func (p *routeClientPool) acquire(ctx context.Context, key routeClientPoolKey, endpoint string) (*bf.Bifrost, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, llm.ErrClientClosed
	}
	if entry := p.entries[key]; entry != nil {
		entry.refs++
		p.lru.MoveToFront(entry.elem)
		p.mu.Unlock()
		return entry.client, p.releaseFunc(entry), nil
	}
	p.mu.Unlock()

	account := newOpenAICompatibleRouteAccount(p.network, endpoint)
	created, err := bf.Init(context.Background(), bfschemas.BifrostConfig{Account: account})
	if err != nil {
		return nil, nil, fmt.Errorf("bifrost: initialize isolated provider route client: %w", err)
	}

	var evicted *bf.Bifrost
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		created.Shutdown()
		return nil, nil, llm.ErrClientClosed
	}
	if entry := p.entries[key]; entry != nil {
		entry.refs++
		p.lru.MoveToFront(entry.elem)
		p.mu.Unlock()
		created.Shutdown()
		return entry.client, p.releaseFunc(entry), nil
	}
	if len(p.entries) >= p.capacity {
		for elem := p.lru.Back(); elem != nil; elem = elem.Prev() {
			candidate, ok := elem.Value.(*routeClientPoolEntry)
			if !ok {
				continue
			}
			if candidate.refs == 0 {
				delete(p.entries, candidate.key)
				p.lru.Remove(elem)
				evicted = candidate.client
				break
			}
		}
		if evicted == nil {
			p.mu.Unlock()
			created.Shutdown()
			return nil, nil, errRouteClientPoolBusy
		}
	}
	entry := &routeClientPoolEntry{key: key, client: created, refs: 1}
	entry.elem = p.lru.PushFront(entry)
	p.entries[key] = entry
	p.mu.Unlock()
	if evicted != nil {
		evicted.Shutdown()
	}
	return created, p.releaseFunc(entry), nil
}

func (p *routeClientPool) releaseFunc(entry *routeClientPoolEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() { p.release(entry) })
	}
}

func (p *routeClientPool) release(entry *routeClientPoolEntry) {
	var shutdown *bf.Bifrost
	p.mu.Lock()
	if entry.refs > 0 {
		entry.refs--
	}
	if p.closed && entry.refs == 0 {
		if current := p.entries[entry.key]; current == entry {
			delete(p.entries, entry.key)
			p.lru.Remove(entry.elem)
			shutdown = entry.client
		}
	}
	p.mu.Unlock()
	if shutdown != nil {
		shutdown.Shutdown()
	}
}

func (p *routeClientPool) Close() {
	if p == nil {
		return
	}
	var shutdown []*bf.Bifrost
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for key, entry := range p.entries {
		if entry.refs != 0 {
			continue
		}
		delete(p.entries, key)
		p.lru.Remove(entry.elem)
		shutdown = append(shutdown, entry.client)
	}
	p.mu.Unlock()
	for _, client := range shutdown {
		client.Shutdown()
	}
}
