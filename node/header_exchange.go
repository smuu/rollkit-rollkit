package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/celestiaorg/go-header"
	goheaderp2p "github.com/celestiaorg/go-header/p2p"
	goheaderstore "github.com/celestiaorg/go-header/store"
	goheadersync "github.com/celestiaorg/go-header/sync"
	ds "github.com/ipfs/go-datastore"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/conngater"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"
	"go.uber.org/multierr"

	"github.com/rollkit/rollkit/config"
	"github.com/rollkit/rollkit/p2p"
	"github.com/rollkit/rollkit/types"
)

type SyncerStatus struct {
	started bool
	m       sync.Mutex
}

type HeaderExchangeService struct {
	conf         config.NodeConfig
	genesis      *tmtypes.GenesisDoc
	p2p          *p2p.Client
	ex           *goheaderp2p.Exchange[*types.SignedHeader]
	syncer       *goheadersync.Syncer[*types.SignedHeader]
	sub          *goheaderp2p.Subscriber[*types.SignedHeader]
	p2pServer    *goheaderp2p.ExchangeServer[*types.SignedHeader]
	headerStore  *goheaderstore.Store[*types.SignedHeader]
	syncerStatus *SyncerStatus

	logger log.Logger
	ctx    context.Context
}

func NewHeaderExchangeService(ctx context.Context, store ds.TxnDatastore, conf config.NodeConfig, genesis *tmtypes.GenesisDoc, p2p *p2p.Client, logger log.Logger) (*HeaderExchangeService, error) {
	// store is TxnDatastore, but we require Batching, hence the type assertion
	// note, the badger datastore impl that is used in the background implements both
	storeBatch, ok := store.(ds.Batching)
	if !ok {
		return nil, errors.New("failed to access the datastore")
	}
	ss, err := goheaderstore.NewStore[*types.SignedHeader](storeBatch)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the header store: %w", err)
	}

	return &HeaderExchangeService{
		conf:         conf,
		genesis:      genesis,
		p2p:          p2p,
		ctx:          ctx,
		headerStore:  ss,
		logger:       logger,
		syncerStatus: new(SyncerStatus),
	}, nil
}

func (hExService *HeaderExchangeService) initHeaderStoreAndStartSyncer(ctx context.Context, initial *types.SignedHeader) error {
	if err := hExService.headerStore.Init(ctx, initial); err != nil {
		return err
	}
	if err := hExService.syncer.Start(hExService.ctx); err != nil {
		return err
	}
	hExService.syncerStatus.m.Lock()
	defer hExService.syncerStatus.m.Unlock()
	hExService.syncerStatus.started = true
	return nil
}

func (hExService *HeaderExchangeService) tryInitHeaderStoreAndStartSyncer(ctx context.Context, trustedHeader *types.SignedHeader) {
	if trustedHeader != nil {
		if err := hExService.initHeaderStoreAndStartSyncer(ctx, trustedHeader); err != nil {
			hExService.logger.Error("failed to initialize the headerstore and start syncer", "error", err)
		}
	}
}

func (hExService *HeaderExchangeService) writeToHeaderStoreAndBroadcast(ctx context.Context, signedHeader *types.SignedHeader) {
	// For genesis header initialize the store and start the syncer
	if signedHeader.Height() == hExService.genesis.InitialHeight {
		if err := hExService.headerStore.Init(ctx, signedHeader); err != nil {
			hExService.logger.Error("failed to initialize header store", "error", err)
		}

		if err := hExService.syncer.Start(hExService.ctx); err != nil {
			hExService.logger.Error("failed to start syncer after initializing header store", "error", err)
		}
	}

	// Broadcast for subscribers
	if err := hExService.sub.Broadcast(ctx, signedHeader); err != nil {
		hExService.logger.Error("failed to broadcast block header", "error", err)
	}
}

// OnStart is a part of Service interface.
func (hExService *HeaderExchangeService) Start() error {
	var err error
	// have to do the initializations here to utilize the p2p node which is created on start
	ps := hExService.p2p.PubSub()
	hExService.sub = goheaderp2p.NewSubscriber[*types.SignedHeader](ps, pubsub.DefaultMsgIdFn, hExService.genesis.ChainID)
	if err = hExService.sub.Start(hExService.ctx); err != nil {
		return fmt.Errorf("error while starting subscriber: %w", err)
	}
	if _, err := hExService.sub.Subscribe(); err != nil {
		return fmt.Errorf("error while subscribing: %w", err)
	}

	if err = hExService.headerStore.Start(hExService.ctx); err != nil {
		return fmt.Errorf("error while starting header store: %w", err)
	}

	_, _, network := hExService.p2p.Info()
	if hExService.p2pServer, err = newP2PServer(hExService.p2p.Host(), hExService.headerStore, network); err != nil {
		return err
	}
	if err = hExService.p2pServer.Start(hExService.ctx); err != nil {
		return fmt.Errorf("error while starting p2p server: %w", err)
	}

	peerIDs := hExService.p2p.PeerIDs()
	// if hExService.ex, err = newP2PExchange(hExService.p2p.Host(), peerIDs, network, hExService.genesis.ChainID, hExService.p2p.ConnectionGater()); err != nil {
	// 	return err
	// }
	if err = hExService.ex.Start(hExService.ctx); err != nil {
		return fmt.Errorf("error while starting exchange: %w", err)
	}

	if hExService.syncer, err = newSyncer(hExService.ex, hExService.headerStore, hExService.sub, goheadersync.WithBlockTime(hExService.conf.BlockTime)); err != nil {
		return err
	}

	// Check if the headerstore is not initialized and try initializing
	if hExService.headerStore.Height() > 0 {
		if err := hExService.syncer.Start(hExService.ctx); err != nil {
			return fmt.Errorf("error while starting the syncer: %w", err)
		}
		hExService.syncerStatus.started = true
		return nil
	}

	// Look to see if trusted hash is passed, if not get the genesis header
	var trustedHeader *types.SignedHeader
	// Try fetching the trusted header from peers if exists
	if len(peerIDs) > 0 {
		if hExService.conf.TrustedHash != "" {
			trustedHashBytes, err := hex.DecodeString(hExService.conf.TrustedHash)
			if err != nil {
				return fmt.Errorf("failed to parse the trusted hash for initializing the headerstore: %w", err)
			}

			if trustedHeader, err = hExService.ex.Get(hExService.ctx, header.Hash(trustedHashBytes)); err != nil {
				return fmt.Errorf("failed to fetch the trusted header for initializing the headerstore: %w", err)
			}
		} else {
			// Try fetching the genesis header if available, otherwise fallback to signed headers
			if trustedHeader, err = hExService.ex.GetByHeight(hExService.ctx, uint64(hExService.genesis.InitialHeight)); err != nil {
				// Full/light nodes have to wait for aggregator to publish the genesis header
				// proposing aggregator can init the store and start the syncer when the first header is published
				hExService.logger.Info("failed to fetch the genesis header", "error", err)
			}
		}
	}
	go hExService.tryInitHeaderStoreAndStartSyncer(hExService.ctx, trustedHeader)

	return nil
}

// OnStop is a part of Service interface.
func (hExService *HeaderExchangeService) Stop() error {
	err := hExService.headerStore.Stop(hExService.ctx)
	err = multierr.Append(err, hExService.p2pServer.Stop(hExService.ctx))
	err = multierr.Append(err, hExService.ex.Stop(hExService.ctx))
	err = multierr.Append(err, hExService.sub.Stop(hExService.ctx))
	hExService.syncerStatus.m.Lock()
	defer hExService.syncerStatus.m.Unlock()
	if hExService.syncerStatus.started {
		err = multierr.Append(err, hExService.syncer.Stop(hExService.ctx))
	}
	return err
}

// newP2PServer constructs a new ExchangeServer using the given Network as a protocolID suffix.
func newP2PServer(
	host host.Host,
	store *goheaderstore.Store[*types.SignedHeader],
	network string,
	opts ...goheaderp2p.Option[goheaderp2p.ServerParameters],
) (*goheaderp2p.ExchangeServer[*types.SignedHeader], error) {
	opts = append(opts,
		goheaderp2p.WithNetworkID[goheaderp2p.ServerParameters](network),
	)
	return goheaderp2p.NewExchangeServer[*types.SignedHeader](host, store, opts...)
}

func newP2PExchange(
	host host.Host,
	peers []peer.ID,
	network, chainID string,
	conngater *conngater.BasicConnectionGater,
	opts ...goheaderp2p.Option[goheaderp2p.ClientParameters],
) (*goheaderp2p.Exchange[*types.SignedHeader], error) {
	opts = append(opts,
		goheaderp2p.WithNetworkID[goheaderp2p.ClientParameters](network),
		goheaderp2p.WithChainID(chainID),
	)
	return goheaderp2p.NewExchange[*types.SignedHeader](host, peers, conngater, opts...)
}

// newSyncer constructs new Syncer for headers.
func newSyncer(
	ex header.Exchange[*types.SignedHeader],
	store header.Store[*types.SignedHeader],
	sub header.Subscriber[*types.SignedHeader],
	opt goheadersync.Options,
) (*goheadersync.Syncer[*types.SignedHeader], error) {
	return goheadersync.NewSyncer[*types.SignedHeader](ex, store, sub, opt)
}
