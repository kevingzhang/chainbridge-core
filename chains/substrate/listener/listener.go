package listener

import (
	"errors"
	"math/big"
	"time"

	"github.com/ChainSafe/chainbridge-core/blockstore"
	"github.com/ChainSafe/chainbridge-core/chains/substrate"
	"github.com/ChainSafe/chainbridge-core/relayer"
	"github.com/centrifuge/go-substrate-rpc-client/types"
	"github.com/rs/zerolog/log"
)

var BlockRetryInterval = time.Second * 5

var ErrBlockNotReady = errors.New("required result to be 32 bytes, but got 0")

type SubstrateReader interface {
	GetHeaderLatest() (*types.Header, error)
	GetBlockHash(blockNumber uint64) (types.Hash, error)
	GetBlockEvents(hash types.Hash, target interface{}) error
	UpdateMetatdata() error
}

type EventHandler func(uint8, interface{}) (*relayer.Message, error)

func NewSubstrateListener(client SubstrateReader) *SubstrateListener {
	return &SubstrateListener{
		client: client,
	}
}

type SubstrateListener struct {
	client        SubstrateReader
	eventHandlers map[relayer.TransferType]EventHandler
}

func (l *SubstrateListener) RegisterSubscription(tt relayer.TransferType, handler EventHandler) {
	if l.eventHandlers == nil {
		l.eventHandlers = make(map[relayer.TransferType]EventHandler)
	}
	l.eventHandlers[tt] = handler
}

func (l *SubstrateListener) ListenToEvents(startBlock *big.Int, chainID uint8, kvrw blockstore.KeyValueWriter, stopChn <-chan struct{}, errChn chan<- error) <-chan *relayer.Message {
	ch := make(chan *relayer.Message)
	go func() {
		for {
			select {
			case <-stopChn:
				return
			default:
				// retrieves the header of the latest block
				finalizedHeader, err := l.client.GetHeaderLatest()
				if err != nil {
					log.Error().Err(err).Msg("Failed to fetch finalized header")
					time.Sleep(BlockRetryInterval)
					continue
				}
				if startBlock.Cmp(big.NewInt(0).SetUint64(uint64(finalizedHeader.Number))) == 1 {
					time.Sleep(BlockRetryInterval)
					continue
				}
				hash, err := l.client.GetBlockHash(startBlock.Uint64())
				if err != nil && err.Error() == ErrBlockNotReady.Error() {
					time.Sleep(BlockRetryInterval)
					continue
				} else if err != nil {
					log.Error().Err(err).Str("block", startBlock.String()).Msg("Failed to query latest block")
					time.Sleep(BlockRetryInterval)
					continue
				}
				evts := &substrate.Events{}
				err = l.client.GetBlockEvents(hash, evts)
				if err != nil {
					log.Error().Err(err).Msg("Failed to process events in block")
					continue
				}
				msg, err := l.handleEvents(chainID, evts)
				if err != nil {
					log.Error().Err(err).Msg("Error handling substrate events")
				}
				for _, m := range msg {
					log.Info().Uint8("chain", chainID).Uint8("destination", m.Destination).Msgf("Sending new message %+v", m)
					ch <- m
				}
				if startBlock.Int64()%20 == 0 {
					// Logging process every 20 blocks to exclude spam
					log.Debug().Str("block", startBlock.String()).Uint8("chainID", chainID).Msg("Queried block for deposit events")
				}
				err = blockstore.StoreBlock(kvrw, startBlock, chainID)
				if err != nil {
					log.Error().Str("block", startBlock.String()).Err(err).Msg("Failed to write latest block to blockstore")
				}
				startBlock.Add(startBlock, big.NewInt(1))
			}
		}
	}()
	return ch
}

// handleEvents calls the associated handler for all registered event types
func (l *SubstrateListener) handleEvents(chainID uint8, evts *substrate.Events) ([]*relayer.Message, error) {
	msgs := make([]*relayer.Message, 0)
	if l.eventHandlers[relayer.FungibleTransfer] != nil {
		for _, evt := range evts.ChainBridge_FungibleTransfer {
			m, err := l.eventHandlers[relayer.FungibleTransfer](chainID, evt)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m)
		}
	}
	if l.eventHandlers[relayer.NonFungibleTransfer] != nil {
		for _, evt := range evts.ChainBridge_NonFungibleTransfer {
			m, err := l.eventHandlers[relayer.NonFungibleTransfer](chainID, evt)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m)

		}
	}
	if l.eventHandlers[relayer.GenericTransfer] != nil {
		for _, evt := range evts.ChainBridge_GenericTransfer {
			m, err := l.eventHandlers[relayer.GenericTransfer](chainID, evt)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m)
		}
	}
	if len(evts.System_CodeUpdated) > 0 {
		err := l.client.UpdateMetatdata()
		if err != nil {
			log.Error().Err(err).Msg("Unable to update Metadata")
			return nil, err
		}
	}
	return msgs, nil
}
