package providers

import (
	"context"
	"time"

	flags "gx/ipfs/QmRMGdC6HKdLsPDABL9aXPDidrpmEHzJqFWSvshkbn9Hj8/go-ipfs-flags"
	process "gx/ipfs/QmSF8fPo3jgVBAy8fpdjjYqgG87dkJgUprRBHRd2tmfgpP/goprocess"
	procctx "gx/ipfs/QmSF8fPo3jgVBAy8fpdjjYqgG87dkJgUprRBHRd2tmfgpP/goprocess/context"
	logging "gx/ipfs/QmTG23dvpBCBjqQwyDxV8CQT6jmS4PSftNr1VqHhE3MLy7/go-log"
	routing "gx/ipfs/QmUHRKTeaoASDvDj7cTAXsmjAY7KQ13ErtzkQHZQq6uFUz/go-libp2p-routing"
	peer "gx/ipfs/QmcJukH2sAFjY3HdBKq35WDzWoL3UUu2gt9wdfqZTUyM74/go-libp2p-peer"
	cid "gx/ipfs/QmcZfnkapfECQGcLZaf9B79NRg7cRa9EnZh4LSbkCzwNvY/go-cid"
	pstore "gx/ipfs/QmdeiKhUy1TVGBaKxt7y1QmBDLBdisSrLJ1x58Eoj4PXUh/go-libp2p-peerstore"
	ipld "gx/ipfs/Qme5bWv7wtjUNGsK2BNGVUFPKiuxWrsqrtvYwCLRw8YFES/go-ipld-format"
	host "gx/ipfs/QmfZTdmunzKzAGJrSvXXQbQ5kLLUiEMX5vdwux7iXkdk7D/go-libp2p-host"
)

const (
	provideTimeout = time.Second * 15

	// MaxProvidersPerRequest specifies the maximum number of providers desired
	// from the network. This value is specified because the network streams
	// results.
	// TODO: if a 'non-nice' strategy is implemented, consider increasing this value
	MaxProvidersPerRequest = 3
	providerRequestTimeout = time.Second * 10

	sizeBatchRequestChan = 32
)

var (
	provideKeysBufferSize = 2048
	// HasBlockBufferSize is the maximum numbers of CIDs that will get buffered
	// for providing
	HasBlockBufferSize = 256

	provideWorkerMax = 512
)

var log = logging.Logger("providers")

type blockRequest struct {
	Cid *cid.Cid
	Ctx context.Context
}

// Interface is an definition of providers interface to libp2p routing system
type Interface interface {
	Provide(k *cid.Cid) error
	ProvideRecursive(ctx context.Context, n ipld.Node, serv ipld.NodeGetter) error

	FindProviders(ctx context.Context, k *cid.Cid) error
	FindProvidersAsync(ctx context.Context, k *cid.Cid, max int) <-chan peer.ID

	Stat() (*Stat, error)
}

type providers struct {
	routing routing.ContentRouting
	process process.Process
	host    host.Host

	// newBlocks is a channel for newly added blocks to be provided to the
	// network.  blocks pushed down this channel get buffered and fed to the
	// provideKeys channel later on to avoid too much network activity
	newBlocks chan *cid.Cid
	// provideKeys directly feeds provide workers
	provideKeys chan *cid.Cid

	// findKeys sends keys to a worker to find and connect to providers for them
	findKeys chan *blockRequest
}

func init() {
	if flags.LowMemMode {
		HasBlockBufferSize = 64
		provideKeysBufferSize = 512
		provideWorkerMax = 16
	}
}

// NewProviders returns providers interface implementation based on
// libp2p routing
func NewProviders(parent context.Context, routing routing.ContentRouting, host host.Host) Interface {
	ctx, cancelFunc := context.WithCancel(parent)

	px := process.WithTeardown(func() error {
		return nil
	})

	p := &providers{
		routing: routing,
		process: px,
		host:    host,

		newBlocks:   make(chan *cid.Cid, HasBlockBufferSize),
		provideKeys: make(chan *cid.Cid, provideKeysBufferSize),

		findKeys: make(chan *blockRequest, sizeBatchRequestChan),
	}

	p.startWorkers(ctx, px)
	// bind the context and process.
	// do it over here to avoid closing before all setup is done.
	go func() {
		<-px.Closing() // process closes first
		cancelFunc()
	}()
	procctx.CloseAfterContext(px, ctx) // parent cancelled first

	return p
}

func (p *providers) Provide(b *cid.Cid) error {
	select {
	case p.newBlocks <- b:
	// send block off to be provided to the network
	case <-p.process.Closing():
		return p.process.Close()
	}
	return nil
}

func (p *providers) provideRecursive(ctx context.Context, n ipld.Node, serv ipld.NodeGetter, done *cid.Set) error {
	p.Provide(n.Cid())

	for _, l := range n.Links() {
		if !done.Visit(l.Cid) {
			continue
		}

		sub, err := l.GetNode(ctx, serv)
		if err != nil {
			return err
		}
		if err := p.provideRecursive(ctx, sub, serv, done); err != nil {
			return err
		}
	}
	return nil
}

func (p *providers) ProvideRecursive(ctx context.Context, n ipld.Node, serv ipld.NodeGetter) error {
	return p.provideRecursive(ctx, n, serv, cid.NewSet())
}

func (p *providers) FindProviders(ctx context.Context, c *cid.Cid) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.findKeys <- &blockRequest{Ctx: ctx, Cid: c}:
		return nil
	}
}

// FindProvidersAsync returns a channel of providers for the given key
func (p *providers) FindProvidersAsync(ctx context.Context, k *cid.Cid, max int) <-chan peer.ID {
	if p.host == nil {
		return nil
	}

	// Since routing queries are expensive, give bitswap the peers to which we
	// have open connections. Note that this may cause issues if bitswap starts
	// precisely tracking which peers provide certain keys. This optimization
	// would be misleading. In the long run, this may not be the most
	// appropriate place for this optimization, but it won't cause any harm in
	// the short term.
	connectedPeers := p.host.Network().Peers()
	out := make(chan peer.ID, len(connectedPeers)) // just enough buffer for these connectedPeers
	for _, id := range connectedPeers {
		if id == p.host.ID() {
			continue // ignore self as provider
		}
		out <- id
	}

	go func() {
		defer close(out)
		providers := p.routing.FindProvidersAsync(ctx, k, max)
		for info := range providers {
			if info.ID == p.host.ID() {
				continue // ignore self as provider
			}
			p.host.Peerstore().AddAddrs(info.ID, info.Addrs, pstore.TempAddrTTL)
			select {
			case <-ctx.Done():
				return
			case out <- info.ID:
			}
		}
	}()
	return out
}
