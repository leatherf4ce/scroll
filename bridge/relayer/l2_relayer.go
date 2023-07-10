package relayer

import (
	"context"
	"errors"
	"math/big"
	"sync"

	"github.com/scroll-tech/go-ethereum/accounts/abi"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/crypto"
	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/log"
	geth_metrics "github.com/scroll-tech/go-ethereum/metrics"

	"scroll-tech/common/metrics"
	"scroll-tech/common/types"

	"scroll-tech/database"

	bridge_abi "scroll-tech/bridge/abi"
	"scroll-tech/bridge/config"
	"scroll-tech/bridge/sender"
	"scroll-tech/bridge/utils"
)

var (
	bridgeL2MsgsRelayedTotalCounter               = geth_metrics.NewRegisteredCounter("bridge/l2/msgs/relayed/total", metrics.ScrollRegistry)
	bridgeL2BatchesFinalizedTotalCounter          = geth_metrics.NewRegisteredCounter("bridge/l2/batches/finalized/total", metrics.ScrollRegistry)
	bridgeL2BatchesCommittedTotalCounter          = geth_metrics.NewRegisteredCounter("bridge/l2/batches/committed/total", metrics.ScrollRegistry)
	bridgeL2MsgsRelayedConfirmedTotalCounter      = geth_metrics.NewRegisteredCounter("bridge/l2/msgs/relayed/confirmed/total", metrics.ScrollRegistry)
	bridgeL2BatchesFinalizedConfirmedTotalCounter = geth_metrics.NewRegisteredCounter("bridge/l2/batches/finalized/confirmed/total", metrics.ScrollRegistry)
	bridgeL2BatchesCommittedConfirmedTotalCounter = geth_metrics.NewRegisteredCounter("bridge/l2/batches/committed/confirmed/total", metrics.ScrollRegistry)
	bridgeL2BatchesSkippedTotalCounter            = geth_metrics.NewRegisteredCounter("bridge/l2/batches/skipped/total", metrics.ScrollRegistry)
)

// Layer2Relayer is responsible for
//  1. Committing and finalizing L2 blocks on L1
//  2. Relaying messages from L2 to L1
//
// Actions are triggered by new head from layer 1 geth node.
// @todo It's better to be triggered by watcher.
type Layer2Relayer struct {
	ctx context.Context

	l2Client *ethclient.Client

	db  database.OrmFactory
	cfg *config.RelayerConfig

	messageSender  *sender.Sender
	l1MessengerABI *abi.ABI

	rollupSender *sender.Sender
	l1RollupABI  *abi.ABI

	gasOracleSender *sender.Sender
	l2GasOracleABI  *abi.ABI

	minGasLimitForMessageRelay uint64

	lastGasPrice uint64
	minGasPrice  uint64
	gasPriceDiff uint64

	// A list of processing message.
	// key(string): confirmation ID, value(string): layer2 hash.
	processingMessage sync.Map

	// A list of processing batches commitment.
	// key(string): confirmation ID, value([]string): batch hashes.
	processingBatchesCommitment sync.Map

	// A list of processing batch finalization.
	// key(string): confirmation ID, value(string): batch hash.
	processingFinalization sync.Map
}

// NewLayer2Relayer will return a new instance of Layer2RelayerClient
func NewLayer2Relayer(ctx context.Context, l2Client *ethclient.Client, db database.OrmFactory, cfg *config.RelayerConfig) (*Layer2Relayer, error) {
	// @todo use different sender for relayer, block commit and proof finalize
	messageSender, err := sender.NewSender(ctx, cfg.SenderConfig, cfg.MessageSenderPrivateKeys)
	if err != nil {
		log.Error("Failed to create messenger sender", "err", err)
		return nil, err
	}

	rollupSender, err := sender.NewSender(ctx, cfg.SenderConfig, cfg.RollupSenderPrivateKeys)
	if err != nil {
		log.Error("Failed to create rollup sender", "err", err)
		return nil, err
	}

	gasOracleSender, err := sender.NewSender(ctx, cfg.SenderConfig, cfg.GasOracleSenderPrivateKeys)
	if err != nil {
		log.Error("Failed to create gas oracle sender", "err", err)
		return nil, err
	}

	var minGasPrice uint64
	var gasPriceDiff uint64
	if cfg.GasOracleConfig != nil {
		minGasPrice = cfg.GasOracleConfig.MinGasPrice
		gasPriceDiff = cfg.GasOracleConfig.GasPriceDiff
	} else {
		minGasPrice = 0
		gasPriceDiff = defaultGasPriceDiff
	}

	minGasLimitForMessageRelay := uint64(defaultL2MessageRelayMinGasLimit)
	if cfg.MessageRelayMinGasLimit != 0 {
		minGasLimitForMessageRelay = cfg.MessageRelayMinGasLimit
	}

	layer2Relayer := &Layer2Relayer{
		ctx: ctx,
		db:  db,

		l2Client: l2Client,

		messageSender:  messageSender,
		l1MessengerABI: bridge_abi.L1ScrollMessengerABI,

		rollupSender: rollupSender,
		l1RollupABI:  bridge_abi.ScrollChainABI,

		gasOracleSender: gasOracleSender,
		l2GasOracleABI:  bridge_abi.L2GasPriceOracleABI,

		minGasLimitForMessageRelay: minGasLimitForMessageRelay,

		minGasPrice:  minGasPrice,
		gasPriceDiff: gasPriceDiff,

		cfg:                         cfg,
		processingMessage:           sync.Map{},
		processingBatchesCommitment: sync.Map{},
		processingFinalization:      sync.Map{},
	}
	go layer2Relayer.handleConfirmLoop(ctx)
	return layer2Relayer, nil
}

const processMsgLimit = 100

// ProcessGasPriceOracle imports gas price to layer1
func (r *Layer2Relayer) ProcessGasPriceOracle() {
	batch, err := r.db.GetLatestBatch()
	if err != nil {
		log.Error("Failed to GetLatestBatch", "err", err)
		return
	}

	if batch.OracleStatus == types.GasOraclePending {
		suggestGasPrice, err := r.l2Client.SuggestGasPrice(r.ctx)
		if err != nil {
			log.Error("Failed to fetch SuggestGasPrice from l2geth", "err", err)
			return
		}
		suggestGasPriceUint64 := uint64(suggestGasPrice.Int64())
		expectedDelta := r.lastGasPrice * r.gasPriceDiff / gasPriceDiffPrecision

		// last is undefine or (suggestGasPriceUint64 >= minGasPrice && exceed diff)
		if r.lastGasPrice == 0 || (suggestGasPriceUint64 >= r.minGasPrice && (suggestGasPriceUint64 >= r.lastGasPrice+expectedDelta || suggestGasPriceUint64 <= r.lastGasPrice-expectedDelta)) {
			data, err := r.l2GasOracleABI.Pack("setL2BaseFee", suggestGasPrice)
			if err != nil {
				log.Error("Failed to pack setL2BaseFee", "batch.Hash", batch.Hash, "GasPrice", suggestGasPrice.Uint64(), "err", err)
				return
			}

			hash, err := r.gasOracleSender.SendTransaction(batch.Hash, &r.cfg.GasPriceOracleContractAddress, big.NewInt(0), data, 0)
			if err != nil {
				if !errors.Is(err, sender.ErrNoAvailableAccount) && !errors.Is(err, sender.ErrFullPending) {
					log.Error("Failed to send setL2BaseFee tx to layer2 ", "batch.Hash", batch.Hash, "err", err)
				}
				return
			}

			err = r.db.UpdateL2GasOracleStatusAndOracleTxHash(r.ctx, batch.Hash, types.GasOracleImporting, hash.String())
			if err != nil {
				log.Error("UpdateGasOracleStatusAndOracleTxHash failed", "batch.Hash", batch.Hash, "err", err)
				return
			}
			r.lastGasPrice = suggestGasPriceUint64
			log.Info("Update l2 gas price", "txHash", hash.String(), "GasPrice", suggestGasPrice)
		}
	}
}

// SendCommitTx sends commitBatches tx to L1.
func (r *Layer2Relayer) SendCommitTx(batchData []*types.BatchData) error {
	if len(batchData) == 0 {
		log.Error("SendCommitTx receives empty batch")
		return nil
	}

	// pack calldata
	commitBatches := make([]bridge_abi.IScrollChainBatch, len(batchData))
	for i, batch := range batchData {
		commitBatches[i] = batch.Batch
	}
	calldata, err := r.l1RollupABI.Pack("commitBatches", commitBatches)
	if err != nil {
		log.Error("Failed to pack commitBatches",
			"error", err,
			"start_batch_index", commitBatches[0].BatchIndex,
			"end_batch_index", commitBatches[len(commitBatches)-1].BatchIndex)
		return err
	}

	// generate a unique txID and send transaction
	var bytes []byte
	for _, batch := range batchData {
		bytes = append(bytes, batch.Hash().Bytes()...)
	}
	txID := crypto.Keccak256Hash(bytes).String()
	txHash, err := r.rollupSender.SendTransaction(txID, &r.cfg.RollupContractAddress, big.NewInt(0), calldata, 0)
	if err != nil {
		if !errors.Is(err, sender.ErrNoAvailableAccount) && !errors.Is(err, sender.ErrFullPending) {
			log.Error("Failed to send commitBatches tx to layer1 ", "err", err)
		}
		return err
	}
	bridgeL2BatchesCommittedTotalCounter.Inc(int64(len(commitBatches)))
	log.Info("Sent the commitBatches tx to layer1",
		"tx_hash", txHash.Hex(),
		"start_batch_index", commitBatches[0].BatchIndex,
		"end_batch_index", commitBatches[len(commitBatches)-1].BatchIndex)

	// record and sync with db, @todo handle db error
	batchHashes := make([]string, len(batchData))
	for i, batch := range batchData {
		batchHashes[i] = batch.Hash().Hex()
		err = r.db.UpdateCommitTxHashAndRollupStatus(r.ctx, batchHashes[i], txHash.String(), types.RollupCommitting)
		if err != nil {
			log.Error("UpdateCommitTxHashAndRollupStatus failed", "hash", batchHashes[i], "index", batch.Batch.BatchIndex, "err", err)
		}
	}
	r.processingBatchesCommitment.Store(txID, batchHashes)
	return nil
}

// ProcessCommittedBatches submit proof to layer 1 rollup contract
func (r *Layer2Relayer) ProcessCommittedBatches() {
	// set skipped batches in a single db operation
	if count, err := r.db.UpdateSkippedBatches(); err != nil {
		log.Error("UpdateSkippedBatches failed", "err", err)
		// continue anyway
	} else if count > 0 {
		bridgeL2BatchesSkippedTotalCounter.Inc(count)
		log.Info("Skipping batches", "count", count)
	}

	// batches are sorted by batch index in increasing order
	batchHashes, err := r.db.GetCommittedBatches(1)
	if err != nil {
		log.Error("Failed to fetch committed L2 batches", "err", err)
		return
	}
	if len(batchHashes) == 0 {
		return
	}
	hash := batchHashes[0]
	// @todo add support to relay multiple batches

	batches, err := r.db.GetBlockBatches(map[string]interface{}{"hash": hash}, "LIMIT 1")
	if err != nil {
		log.Error("Failed to fetch committed L2 batch", "hash", hash, "err", err)
		return
	}
	if len(batches) == 0 {
		log.Error("Unexpected result for GetBlockBatches", "hash", hash, "len", 0)
		return
	}

	batch := batches[0]
	status := batch.ProvingStatus

	switch status {
	case types.ProvingTaskUnassigned, types.ProvingTaskAssigned:
		// The proof for this block is not ready yet.
		return

	case types.ProvingTaskProved:
		// It's an intermediate state. The roller manager received the proof but has not verified
		// the proof yet. We don't roll up the proof until it's verified.
		return

	case types.ProvingTaskFailed, types.ProvingTaskSkipped:
		// note: this is covered by UpdateSkippedBatches, but we keep it for completeness's sake

		if err = r.db.UpdateRollupStatus(r.ctx, hash, types.RollupFinalizationSkipped); err != nil {
			log.Warn("UpdateRollupStatus failed", "hash", hash, "err", err)
		}

	case types.ProvingTaskVerified:
		log.Info("Start to roll up zk proof", "hash", hash)
		success := false

		previousBatch, err := r.db.GetLatestFinalizingOrFinalizedBatch()

		// skip submitting proof
		if err == nil && uint64(batch.CreatedAt.Sub(*previousBatch.CreatedAt).Seconds()) < r.cfg.FinalizeBatchIntervalSec {
			log.Info(
				"Not enough time passed, skipping",
				"hash", hash,
				"createdAt", batch.CreatedAt,
				"lastFinalizingHash", previousBatch.Hash,
				"lastFinalizingStatus", previousBatch.RollupStatus,
				"lastFinalizingCreatedAt", previousBatch.CreatedAt,
			)

			if err = r.db.UpdateRollupStatus(r.ctx, hash, types.RollupFinalizationSkipped); err != nil {
				log.Warn("UpdateRollupStatus failed", "hash", hash, "err", err)
			} else {
				success = true
			}

			return
		}

		// handle unexpected db error
		if err != nil && err.Error() != "sql: no rows in result set" {
			log.Error("Failed to get latest finalized batch", "err", err)
			return
		}

		defer func() {
			// TODO: need to revisit this and have a more fine-grained error handling
			if !success {
				log.Info("Failed to upload the proof, change rollup status to FinalizationSkipped", "hash", hash)
				if err = r.db.UpdateRollupStatus(r.ctx, hash, types.RollupFinalizationSkipped); err != nil {
					log.Warn("UpdateRollupStatus failed", "hash", hash, "err", err)
				}
			}
		}()

		proofBuffer, icBuffer, err := r.db.GetVerifiedProofAndInstanceCommitmentsByHash(hash)
		if err != nil {
			log.Warn("fetch get proof by hash failed", "hash", hash, "err", err)
			return
		}
		if proofBuffer == nil || icBuffer == nil {
			log.Warn("proof or instance not ready", "hash", hash)
			return
		}
		if len(proofBuffer)%32 != 0 {
			log.Error("proof buffer has wrong length", "hash", hash, "length", len(proofBuffer))
			return
		}
		if len(icBuffer)%32 != 0 {
			log.Warn("instance buffer has wrong length", "hash", hash, "length", len(icBuffer))
			return
		}

		proof := utils.BufferToUint256Le(proofBuffer)
		instance := utils.BufferToUint256Le(icBuffer)
		data, err := r.l1RollupABI.Pack("finalizeBatchWithProof", common.HexToHash(hash), proof, instance)
		if err != nil {
			log.Error("Pack finalizeBatchWithProof failed", "err", err)
			return
		}

		txID := hash + "-finalize"
		// add suffix `-finalize` to avoid duplication with commit tx in unit tests
		txHash, err := r.rollupSender.SendTransaction(txID, &r.cfg.RollupContractAddress, big.NewInt(0), data, 0)
		finalizeTxHash := &txHash
		if err != nil {
			if !errors.Is(err, sender.ErrNoAvailableAccount) && !errors.Is(err, sender.ErrFullPending) {
				log.Error("finalizeBatchWithProof in layer1 failed", "hash", hash, "err", err)
			}
			return
		}
		bridgeL2BatchesFinalizedTotalCounter.Inc(1)
		log.Info("finalizeBatchWithProof in layer1", "batch_hash", hash, "tx_hash", hash)

		// record and sync with db, @todo handle db error
		err = r.db.UpdateFinalizeTxHashAndRollupStatus(r.ctx, hash, finalizeTxHash.String(), types.RollupFinalizing)
		if err != nil {
			log.Warn("UpdateFinalizeTxHashAndRollupStatus failed", "batch_hash", hash, "err", err)
		}
		success = true
		r.processingFinalization.Store(txID, hash)

	default:
		log.Error("encounter unreachable case in ProcessCommittedBatches",
			"block_status", status,
		)
	}
}

func (r *Layer2Relayer) handleConfirmation(confirmation *sender.Confirmation) {
	transactionType := "Unknown"
	// check whether it is message relay transaction
	if msgHash, ok := r.processingMessage.Load(confirmation.ID); ok {
		transactionType = "MessageRelay"
		var status types.MsgStatus
		if confirmation.IsSuccessful {
			status = types.MsgConfirmed
		} else {
			status = types.MsgRelayFailed
			log.Warn("transaction confirmed but failed in layer1", "confirmation", confirmation)
		}
		// @todo handle db error
		err := r.db.UpdateLayer2StatusAndLayer1Hash(r.ctx, msgHash.(string), status, confirmation.TxHash.String())
		if err != nil {
			log.Warn("UpdateLayer2StatusAndLayer1Hash failed", "msgHash", msgHash.(string), "err", err)
		}
		bridgeL2MsgsRelayedConfirmedTotalCounter.Inc(1)
		r.processingMessage.Delete(confirmation.ID)
	}

	// check whether it is CommitBatches transaction
	if batchBatches, ok := r.processingBatchesCommitment.Load(confirmation.ID); ok {
		transactionType = "BatchesCommitment"
		batchHashes := batchBatches.([]string)
		var status types.RollupStatus
		if confirmation.IsSuccessful {
			status = types.RollupCommitted
		} else {
			status = types.RollupCommitFailed
			log.Warn("transaction confirmed but failed in layer1", "confirmation", confirmation)
		}
		for _, batchHash := range batchHashes {
			// @todo handle db error
			err := r.db.UpdateCommitTxHashAndRollupStatus(r.ctx, batchHash, confirmation.TxHash.String(), status)
			if err != nil {
				log.Warn("UpdateCommitTxHashAndRollupStatus failed", "batch_hash", batchHash, "err", err)
			}
		}
		bridgeL2BatchesCommittedConfirmedTotalCounter.Inc(int64(len(batchHashes)))
		r.processingBatchesCommitment.Delete(confirmation.ID)
	}

	// check whether it is proof finalization transaction
	if batchHash, ok := r.processingFinalization.Load(confirmation.ID); ok {
		transactionType = "ProofFinalization"
		var status types.RollupStatus
		if confirmation.IsSuccessful {
			status = types.RollupFinalized
		} else {
			status = types.RollupFinalizeFailed
			log.Warn("transaction confirmed but failed in layer1", "confirmation", confirmation)
		}
		// @todo handle db error
		err := r.db.UpdateFinalizeTxHashAndRollupStatus(r.ctx, batchHash.(string), confirmation.TxHash.String(), status)
		if err != nil {
			log.Warn("UpdateFinalizeTxHashAndRollupStatus failed", "batch_hash", batchHash.(string), "err", err)
		}
		bridgeL2BatchesFinalizedConfirmedTotalCounter.Inc(1)
		r.processingFinalization.Delete(confirmation.ID)
	}
	log.Info("transaction confirmed in layer1", "type", transactionType, "confirmation", confirmation)
}

func (r *Layer2Relayer) handleConfirmLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case confirmation := <-r.messageSender.ConfirmChan():
			r.handleConfirmation(confirmation)
		case confirmation := <-r.rollupSender.ConfirmChan():
			r.handleConfirmation(confirmation)
		case cfm := <-r.gasOracleSender.ConfirmChan():
			if !cfm.IsSuccessful {
				// @discuss: maybe make it pending again?
				err := r.db.UpdateL2GasOracleStatusAndOracleTxHash(r.ctx, cfm.ID, types.GasOracleFailed, cfm.TxHash.String())
				if err != nil {
					log.Warn("UpdateL2GasOracleStatusAndOracleTxHash failed", "err", err)
				}
				log.Warn("transaction confirmed but failed in layer1", "confirmation", cfm)
			} else {
				// @todo handle db error
				err := r.db.UpdateL2GasOracleStatusAndOracleTxHash(r.ctx, cfm.ID, types.GasOracleImported, cfm.TxHash.String())
				if err != nil {
					log.Warn("UpdateL2GasOracleStatusAndOracleTxHash failed", "err", err)
				}
				log.Info("transaction confirmed in layer1", "confirmation", cfm)
			}
		}
	}
}
