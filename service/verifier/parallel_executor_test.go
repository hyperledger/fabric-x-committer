/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

func TestConfigTransactionImmediateProcessing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	executor := startExecutor(ctx, t, 100)
	executor.inputCh <- newTx("config-tx-1", 0, 0, committerpb.ConfigNamespaceID)

	select {
	case result := <-executor.outputCh:
		require.Len(t, result, 1)
	case <-ctx.Done():
		t.Fatal("Config transaction was not returned immediately - batching delay detected")
	}
}

func TestRegularTransactionBatching(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	executor := startExecutor(ctx, t, 2)
	executor.inputCh <- newTx("regular-tx-1", 0, 0, "namespace1")

	select {
	case <-executor.outputCh:
		t.Fatal("Regular transaction was returned before batch size cutoff was reached")
	case <-time.After(500 * time.Millisecond):
	}

	executor.inputCh <- newTx("regular-tx-2", 0, 1, "namespace1")

	select {
	case result := <-executor.outputCh:
		require.Len(t, result, 2)
	case <-ctx.Done():
		t.Fatal("Batch was not returned after reaching batch size cutoff")
	}
}

func startExecutor(ctx context.Context, t *testing.T, batchSizeCutoff int) *parallelExecutor {
	t.Helper()
	config := &ExecutorConfig{
		BatchSizeCutoff:   batchSizeCutoff,
		BatchTimeCutoff:   1 * time.Hour,
		Parallelism:       1,
		ChannelBufferSize: 10,
	}
	executor := newParallelExecutor(config)

	go executor.handleChannelInput(ctx)
	go executor.handleCutoff(ctx)

	return executor
}

func newTx(txID string, blockNum uint64, txNum uint32, nsID string) *servicepb.TxWithRef {
	return &servicepb.TxWithRef{
		Ref: committerpb.NewTxRef(txID, blockNum, txNum),
		Content: &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:      nsID,
				NsVersion: 0,
				BlindWrites: []*applicationpb.Write{{
					Key:   []byte("key"),
					Value: []byte("value"),
				}},
			}},
		},
	}
}
