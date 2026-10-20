/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/jackc/puddle/v2"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// ErrConfigUnavailable is returned before any channel-configuration bundle has loaded, so nothing can be
// authenticated or authorized yet. Expected during bootstrap, before the first config block commits.
var ErrConfigUnavailable = errors.New("channel configuration not available")

// configProvider exposes the latest committed channel configuration as a bundle; it never mutates it. The
// bundle is swapped atomically.
type configProvider struct {
	db      *database
	metrics *perfMetrics

	bundle      atomic.Pointer[channelconfig.Bundle]
	lastVersion uint64
	seen        bool
}

// run reads the configuration now, then every interval.
func (p *configProvider) run(ctx context.Context, interval time.Duration) error {
	if err := p.refresh(ctx); err != nil {
		logger.Warnf("Initial channel-configuration load failed (will retry): %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.refresh(ctx); err != nil {
				logger.Errorf("Channel-configuration refresh failed: %v", err)
			}
		}
	}
}

// refresh installs a new bundle only when the committed configuration's version has advanced.
func (p *configProvider) refresh(ctx context.Context) error {
	configTX, err := retry.ExecuteWithResult(ctx, p.db.retry, func() (*applicationpb.ConfigTransaction, error) {
		return statedb.ReadConfigTransaction(ctx, p.db.pool)
	}, puddle.ErrClosedPool)
	if err != nil {
		return err
	}

	promutil.SetGauge(p.metrics.configLastRefresh, int(time.Now().Unix()))
	if len(configTX.GetEnvelope()) == 0 {
		return nil // No configuration committed yet.
	}
	if p.seen && configTX.GetVersion() <= p.lastVersion {
		return nil // Not newer than what we already hold.
	}

	envelope, err := protoutil.UnmarshalEnvelope(configTX.GetEnvelope())
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal config envelope")
	}
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return errors.Wrap(err, "failed to build channel configuration bundle")
	}

	sequence := bundle.ConfigtxValidator().Sequence()
	p.bundle.Store(bundle)
	p.lastVersion = configTX.GetVersion()
	p.seen = true
	logger.Infof("Loaded channel configuration version %d (sequence %d)", configTX.GetVersion(), sequence)
	return nil
}

// current returns the latest loaded bundle, or ErrConfigUnavailable if none has been loaded yet.
func (p *configProvider) current() (*channelconfig.Bundle, error) {
	bundle := p.bundle.Load()
	if bundle == nil {
		return nil, ErrConfigUnavailable
	}
	return bundle, nil
}
