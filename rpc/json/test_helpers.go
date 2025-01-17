package json

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/proxy"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/rollkit/rollkit/config"
	"github.com/rollkit/rollkit/conv"
	"github.com/rollkit/rollkit/mocks"
	"github.com/rollkit/rollkit/node"
)

// copied from rpc
func getRPC(t *testing.T) (*mocks.Application, rpcclient.Client) {
	t.Helper()
	require := require.New(t)
	app := &mocks.Application{}
	app.On("InitChain", mock.Anything).Return(abci.ResponseInitChain{})
	app.On("BeginBlock", mock.Anything).Return(abci.ResponseBeginBlock{})
	app.On("EndBlock", mock.Anything).Return(abci.ResponseEndBlock{})
	app.On("Commit", mock.Anything).Return(abci.ResponseCommit{})
	app.On("GetAppHash", mock.Anything).Return(abci.ResponseGetAppHash{})
	app.On("GenerateFraudProof", mock.Anything).Return(abci.ResponseGenerateFraudProof{})
	app.On("CheckTx", mock.Anything).Return(abci.ResponseCheckTx{
		GasWanted: 1000,
		GasUsed:   1000,
	})
	app.On("Info", mock.Anything).Return(abci.ResponseInfo{
		Data:             "mock",
		Version:          "mock",
		AppVersion:       123,
		LastBlockHeight:  345,
		LastBlockAppHash: nil,
	})
	key, _, _ := crypto.GenerateEd25519Key(rand.Reader)
	validatorKey := ed25519.GenPrivKey()
	nodeKey := &p2p.NodeKey{
		PrivKey: validatorKey,
	}
	signingKey, _ := conv.GetNodeKey(nodeKey)
	pubKey := validatorKey.PubKey()

	genesisValidators := []tmtypes.GenesisValidator{
		{Address: pubKey.Address(), PubKey: pubKey, Power: int64(100), Name: "gen #1"},
	}
	n, err := node.NewNode(context.Background(), config.NodeConfig{Aggregator: true, DALayer: "mock", BlockManagerConfig: config.BlockManagerConfig{BlockTime: 1 * time.Second}, Light: false}, key, signingKey, proxy.NewLocalClientCreator(app), &tmtypes.GenesisDoc{ChainID: "test", Validators: genesisValidators}, log.TestingLogger())
	require.NoError(err)
	require.NotNil(n)

	err = n.Start()
	require.NoError(err)

	local := n.GetClient()
	require.NotNil(local)

	return app, local
}
