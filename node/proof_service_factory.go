package node

import (
	"github.com/celestiaorg/go-fraud"
	"github.com/celestiaorg/go-fraud/fraudserv"
	"github.com/ipfs/go-datastore"

	"github.com/rollkit/rollkit/p2p"
)

type ProofServiceFactory struct {
	client        *p2p.Client
	getter        fraud.HeaderFetcher
	ds            datastore.Datastore
	syncerEnabled bool
	networkID     string
}

func NewProofServiceFactory(c *p2p.Client, getter fraud.HeaderFetcher, ds datastore.Datastore, syncerEnabled bool, networkID string) ProofServiceFactory {
	return ProofServiceFactory{
		client:        c,
		getter:        getter,
		ds:            ds,
		syncerEnabled: syncerEnabled,
		networkID:     networkID,
	}
}

func (factory *ProofServiceFactory) CreateProofService() *fraudserv.ProofService {
	return fraudserv.NewProofService(
		factory.client.PubSub(),
		factory.client.Host(),
		factory.getter,
		factory.ds,
		factory.syncerEnabled,
		factory.networkID,
	)
}
