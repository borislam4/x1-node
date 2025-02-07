package sequencer

import (
	"context"
	"fmt"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	stateMetrics "github.com/0xPolygonHermez/zkevm-node/state/metrics"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
)

// processForcedBatches processes all the forced batches that are pending to be processed
func (f *finalizer) processForcedBatches(ctx context.Context, lastBatchNumber uint64, stateRoot, accInputHash common.Hash) (newLastBatchNumber uint64, newStateRoot, newAccInputHash common.Hash) {
	f.nextForcedBatchesMux.Lock()
	defer f.nextForcedBatchesMux.Unlock()
	f.nextForcedBatchDeadline = 0

	lastForcedBatchNumber, err := f.state.GetLastTrustedForcedBatchNumber(ctx, nil)
	if err != nil {
		log.Errorf("[processForcedBatches] failed to get last trusted forced batch number. Error: %w", err)
		return lastBatchNumber, stateRoot, accInputHash
	}
	nextForcedBatchNumber := lastForcedBatchNumber + 1

	for _, forcedBatch := range f.nextForcedBatches {
		forcedBatchToProcess := forcedBatch
		// Skip already processed forced batches
		if forcedBatchToProcess.ForcedBatchNumber < nextForcedBatchNumber {
			continue
		} else if forcedBatch.ForcedBatchNumber > nextForcedBatchNumber {
			// We have a gap in the f.nextForcedBatches slice, we get the missing forced batch from the state
			missingForcedBatch, err := f.state.GetForcedBatch(ctx, nextForcedBatchNumber, nil)
			if err != nil {
				log.Errorf("[processForcedBatches] failed to get missing forced batch %d. Error: %w", nextForcedBatchNumber, err)
				return lastBatchNumber, stateRoot, accInputHash
			}
			forcedBatchToProcess = *missingForcedBatch
		}

		log.Infof("processing forced batch %d, LastBatchNumber: %d, StateRoot: %s, AccInputHash: %s", forcedBatchToProcess.ForcedBatchNumber, lastBatchNumber, stateRoot.String(), accInputHash.String())
		lastBatchNumber, stateRoot, accInputHash, err = f.processForcedBatch(ctx, forcedBatchToProcess, lastBatchNumber, stateRoot, accInputHash)

		if err != nil {
			log.Errorf("[processForcedBatches] error when processing forced batch %d. Error: %w", forcedBatchToProcess.ForcedBatchNumber, err)
			return lastBatchNumber, stateRoot, accInputHash
		}

		log.Infof("processed forced batch %d, BatchNumber: %d, NewStateRoot: %s, NewAccInputHash: %s", forcedBatchToProcess.ForcedBatchNumber, lastBatchNumber, stateRoot.String(), accInputHash.String())

		nextForcedBatchNumber += 1
	}
	f.nextForcedBatches = make([]state.ForcedBatch, 0)

	return lastBatchNumber, stateRoot, accInputHash
}

func (f *finalizer) processForcedBatch(ctx context.Context, forcedBatch state.ForcedBatch, lastBatchNumber uint64, stateRoot, accInputHash common.Hash) (newLastBatchNumber uint64, newStateRoot, newAccInputHash common.Hash, retErr error) {
	dbTx, err := f.state.BeginStateTransaction(ctx)
	if err != nil {
		log.Errorf("failed to begin state transaction for process forced batch %d. Error: %w", forcedBatch.ForcedBatchNumber, err)
		return lastBatchNumber, stateRoot, accInputHash, err
	}

	// Helper function in case we get an error when processing the forced batch
	rollbackOnError := func(retError error) (newLastBatchNumber uint64, newStateRoot, newAccInputHash common.Hash, retErr error) {
		err := dbTx.Rollback(ctx)
		if err != nil {
			return lastBatchNumber, stateRoot, accInputHash, fmt.Errorf("[processForcedBatch] rollback error due to error %w. Error: %w", retError, err)
		}
		return lastBatchNumber, stateRoot, accInputHash, retError
	}

	// Get L1 block for the forced batch
	fbL1Block, err := f.state.GetBlockByNumber(ctx, forcedBatch.ForcedBatchNumber, dbTx)
	if err != nil {
		return lastBatchNumber, stateRoot, accInputHash, fmt.Errorf("[processForcedBatch] error getting L1 block number %d for forced batch %d. Error: %w", forcedBatch.ForcedBatchNumber, forcedBatch.ForcedBatchNumber, err)
	}

	newBatchNumber := lastBatchNumber + 1

	// Open new batch on state for the forced batch
	processingCtx := state.ProcessingContext{
		BatchNumber:    newBatchNumber,
		Coinbase:       f.sequencerAddress,
		Timestamp:      time.Now(),
		GlobalExitRoot: forcedBatch.GlobalExitRoot,
		ForcedBatchNum: &forcedBatch.ForcedBatchNumber,
	}
	err = f.state.OpenBatch(ctx, processingCtx, dbTx)
	if err != nil {
		return rollbackOnError(fmt.Errorf("[processForcedBatch] error opening state batch %d for forced batch %d. Error: %w", newBatchNumber, forcedBatch.ForcedBatchNumber, err))
	}

	executorBatchRequest := state.ProcessRequest{
		BatchNumber:             newBatchNumber,
		L1InfoRoot_V2:           forcedBatch.GlobalExitRoot,
		ForcedBlockHashL1:       fbL1Block.ParentHash,
		OldStateRoot:            stateRoot,
		OldAccInputHash:         accInputHash,
		Transactions:            forcedBatch.RawTxsData,
		Coinbase:                f.sequencerAddress,
		TimestampLimit_V2:       uint64(forcedBatch.ForcedAt.Unix()),
		ForkID:                  f.state.GetForkIDByBatchNumber(lastBatchNumber),
		SkipVerifyL1InfoRoot_V2: true,
		Caller:                  stateMetrics.SequencerCallerLabel,
	}

	// falta pasar timestamp_limit = fb.ForcedAt
	// L1InfoRoot = fb.GER
	// forced_blockhash_l1 = table.forced_batch.block_num.parent_hash
	// l1_info_tree_data  vacio
	batchResponse, err := f.state.ProcessBatchV2(ctx, executorBatchRequest, true)
	if err != nil {
		return rollbackOnError(fmt.Errorf("[processForcedBatch] failed to process/execute forced batch %d. Error: %w", forcedBatch.ForcedBatchNumber, err))
	}

	// Close state batch
	processingReceipt := state.ProcessingReceipt{
		BatchNumber:   newBatchNumber,
		StateRoot:     batchResponse.NewStateRoot,
		LocalExitRoot: batchResponse.NewLocalExitRoot,
		AccInputHash:  batchResponse.NewAccInputHash,
		BatchL2Data:   forcedBatch.RawTxsData,
		BatchResources: state.BatchResources{
			ZKCounters: batchResponse.UsedZkCounters,
			Bytes:      uint64(len(forcedBatch.RawTxsData)),
		},
		ClosingReason: state.ForcedBatchClosingReason,
	}
	err = f.state.CloseBatch(ctx, processingReceipt, dbTx)
	if err != nil {
		return rollbackOnError(fmt.Errorf("[processForcedBatch] error closing state batch %d for forced batch %d. Error: %w", newBatchNumber, forcedBatch.ForcedBatchNumber, err))
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return rollbackOnError(fmt.Errorf("[processForcedBatch] error when commit dbTx when processing forced batch %d. Error: %w", forcedBatch.ForcedBatchNumber, err))
	}

	if len(batchResponse.BlockResponses) > 0 && !batchResponse.IsRomOOCError {
		err = f.handleProcessForcedBatchResponse(ctx, batchResponse, dbTx)
		return rollbackOnError(fmt.Errorf("[processForcedBatch] error when handling batch response for forced batch %d. Error: %w", forcedBatch.ForcedBatchNumber, err))
	} //else {
	//TODO: review if this is still needed
	/*if f.streamServer != nil && f.currentGERHash != forcedBatch.GlobalExitRoot {
		//TODO: review this datastream parameters
		f.DSSendUpdateGER(newBatchNumber, forcedBatch.ForcedAt.Unix(), forcedBatch.GlobalExitRoot, batchResponse.NewStateRoot)
	}*/
	//}

	return newBatchNumber, batchResponse.NewStateRoot, batchResponse.NewAccInputHash, nil
}

// addForcedTxToWorker adds the txs of the forced batch to the worker
func (f *finalizer) addForcedTxToWorker(forcedBatchResponse *state.ProcessBatchResponse) {
	for _, blockResponse := range forcedBatchResponse.BlockResponses {
		for _, txResponse := range blockResponse.TransactionResponses {
			from, err := state.GetSender(txResponse.Tx)
			if err != nil {
				log.Warnf("failed trying to add forced tx (%s) to worker. Error getting sender from tx, Error: %w", txResponse.TxHash, err)
				continue
			}
			f.worker.AddForcedTx(txResponse.TxHash, from)
		}
	}
}

// handleProcessForcedTxsResponse handles the block/transactions responses for the processed forced batch.
func (f *finalizer) handleProcessForcedBatchResponse(ctx context.Context, batchResponse *state.ProcessBatchResponse, dbTx pgx.Tx) error {
	f.addForcedTxToWorker(batchResponse)

	f.updateLastPendingFlushID(batchResponse.FlushID)

	// Wait until forced batch has been flushed/stored by the executor
	f.storedFlushIDCond.L.Lock()
	for f.storedFlushID < batchResponse.FlushID {
		f.storedFlushIDCond.Wait()
		// check if context is done after waking up
		if ctx.Err() != nil {
			f.storedFlushIDCond.L.Unlock()
			return nil
		}
	}
	f.storedFlushIDCond.L.Unlock()

	// process L2 blocks responses for the forced batch
	for _, forcedL2BlockResponse := range batchResponse.BlockResponses {
		// Store forced L2 blocks in the state
		err := f.state.StoreL2Block(ctx, batchResponse.NewBatchNumber, forcedL2BlockResponse, nil, dbTx)
		if err != nil {
			return fmt.Errorf("[handleProcessForcedBatchResponse] database error on storing L2 block %d. Error: %w", forcedL2BlockResponse.BlockNumber, err)
		}

		// Update worker with info from the transaction responses
		for _, txResponse := range forcedL2BlockResponse.TransactionResponses {
			from, err := state.GetSender(txResponse.Tx)
			if err != nil {
				log.Warnf("[handleForcedTxsProcessResp] failed to get sender for tx (%s): %v", txResponse.TxHash, err)
			}

			if err == nil {
				f.updateWorkerAfterSuccessfulProcessing(ctx, txResponse.TxHash, from, true, batchResponse)
			}
		}

		// Send L2 block to data streamer
		err = f.DSSendL2Block(batchResponse.NewBatchNumber, forcedL2BlockResponse)
		if err != nil {
			//TODO: we need to halt/rollback the L2 block if we had an error sending to the data streamer?
			log.Errorf("[storeL2Block] error sending L2 block %d to data streamer", forcedL2BlockResponse.BlockNumber)
		}
	}

	return nil
}
