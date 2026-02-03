/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package loadgen

import (
	"context"
	"net/http"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

type (
	// Client for applying load on the services.
	Client struct {
		servicepb.UnimplementedLoadGenServiceServer
		conf        *ClientConfig
		txStream    *workload.TxStream
		resources   adapters.ClientResources
		adapter     ServiceAdapter
		healthcheck *health.Server
		ready       *channel.Ready
	}

	// ServiceAdapter encapsulates the common interface for adapters.
	ServiceAdapter interface {
		// RunWorkload apply the generated workload.
		RunWorkload(ctx context.Context, txStream *workload.StreamWithSetup) error
		// Progress returns a value that indicates progress as the number grows.
		Progress() uint64
		// Supports specify which phases an adapter supports.
		Supports() adapters.Phases
	}
)

var logger = logging.New("load-gen-client")

// NewLoadGenClient creates a new client instance.
func NewLoadGenClient(conf *ClientConfig) (*Client, error) {
	logger.Debugf("Config passed: %s", &utils.LazyJSON{O: conf})

	c := &Client{
		conf: conf,
		resources: adapters.ClientResources{
			Profile: conf.LoadProfile,
			Stream:  conf.Stream,
			Limit:   conf.Limit,
			Metrics: metrics.NewLoadgenServiceMetrics(&conf.Monitoring),
		},
		healthcheck: connection.DefaultHealthCheckService(),
		ready:       channel.NewReady(),
	}

	adapter, err := getAdapter(&conf.Adapter, &c.resources)
	if err != nil {
		return nil, err
	}

	// We generate the crypto material and block after we create the adapter since the sidecar adapter
	// modifies the orderer endpoints.
	c.resources.ConfigBlock, err = workload.CreateConfigBlock(&conf.LoadProfile.Policy)
	if err != nil {
		return nil, err
	}

	// After creating the material, we can create the stream.
	c.txStream = workload.NewTxStream(conf.LoadProfile, conf.Stream)

	c.adapter = adapter
	conf.Generate = adapters.PhasesIntersect(conf.Generate, adapter.Supports())
	return c, nil
}

func getAdapter(conf *adapters.AdapterConfig, res *adapters.ClientResources) (ServiceAdapter, error) {
	switch {
	case conf.CoordinatorClient != nil:
		return adapters.NewCoordinatorAdapter(conf.CoordinatorClient, res), nil
	case conf.VCClient != nil:
		return adapters.NewVCAdapter(conf.VCClient, res), nil
	case conf.OrdererClient != nil:
		return adapters.NewOrdererAdapter(conf.OrdererClient, res), nil
	case conf.SidecarClient != nil:
		return adapters.NewSidecarAdapter(conf.SidecarClient, res)
	case conf.VerifierClient != nil:
		return adapters.NewSVAdapter(conf.VerifierClient, res), nil
	case conf.LoadGenClient != nil:
		return adapters.NewLoadGenAdapter(conf.LoadGenClient, res), nil
	default:
		return nil, adapters.ErrInvalidAdapterConfig
	}
}

// Run applies load on the service.
func (c *Client) Run(ctx context.Context) error {
	logger.Infof("Starting workload generation")
	defer logger.Infof("End workload generation")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return c.resources.Metrics.StartPrometheusServer(gCtx, c.conf.Monitoring.Server)
	})
	g.Go(func() error {
		return c.runHTTPServer(ctx)
	})
	g.Go(func() error {
		return c.txStream.Run(gCtx)
	})

	defer c.ready.Reset()
	c.ready.SignalReady()

	workloadSetupTXs := make(chan *servicepb.LoadGenTx, 1)
	cs := &workload.StreamWithSetup{
		WorkloadSetupTXs: channel.NewReader(gCtx, workloadSetupTXs),
	}
	if c.conf.Generate.Load {
		cs.TxStream = c.txStream
	}
	g.Go(func() error {
		defer cancel() // We should stop if the workload is over.
		return c.adapter.RunWorkload(gCtx, cs)
	})

	if err := c.submitWorkloadSetupTXs(gCtx, workloadSetupTXs); err != nil {
		logger.Errorf("Failed to process tx: %+v", err)
		cancel()
		if gErr := g.Wait(); gErr != nil {
			return errors.Wrap(gErr, "failed to process TX")
		}
		return err
	}

	if !c.conf.Generate.Load {
		cancel()
	}
	return g.Wait()
}

// WaitForReady waits for the service resources to initialize, so it is ready to answers requests.
// If the context ended before the service is ready, returns false.
func (c *Client) WaitForReady(ctx context.Context) bool {
	return c.ready.WaitForReady(ctx)
}

// RegisterService registers for the load-gen's GRPC services.
func (c *Client) RegisterService(server *grpc.Server) {
	servicepb.RegisterLoadGenServiceServer(server, c)
	healthgrpc.RegisterHealthServer(server, c.healthcheck)
}

// AppendBatch appends a batch to the stream.
func (c *Client) AppendBatch(ctx context.Context, batch *servicepb.LoadGenBatch) (*emptypb.Empty, error) {
	c.txStream.AppendBatch(ctx, batch.Tx)
	return nil, nil
}

// GetRateLimit reads the stream limit.
func (c *Client) GetRateLimit(context.Context, *emptypb.Empty) (*servicepb.RateLimit, error) {
	return &servicepb.RateLimit{Rate: c.txStream.GetRate()}, nil
}

// SetRateLimit sets the stream limit.
func (c *Client) SetRateLimit(_ context.Context, limit *servicepb.RateLimit) (*emptypb.Empty, error) {
	c.txStream.SetRate(limit.Rate)
	return nil, nil
}

// runHTTPServer starts a simple HTTP server for setting the rate limit.
func (c *Client) runHTTPServer(ctx context.Context) error {
	serverConfig := c.conf.HTTPServer
	if serverConfig == nil || serverConfig.Endpoint.Empty() {
		return nil
	}

	tlsMaterials, err := connection.NewTLSMaterials(serverConfig.TLS)
	if err != nil {
		return err
	}
	tlsCfg, err := tlsMaterials.CreateServerTLSConfig()
	if err != nil {
		return err
	}

	mux := runtime.NewServeMux()
	err = servicepb.RegisterLoadGenServiceHandlerServer(ctx, mux, c)
	if err != nil {
		return errors.Wrap(err, "failed to register loadgen service handler")
	}

	g, gCtx := errgroup.WithContext(ctx)
	ln, err := serverConfig.Listener(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to listen")
	}
	defer func() {
		_ = ln.Close()
	}()

	logger.Infof("Start remote controller server on %s", serverConfig.Endpoint.Address())
	server := &http.Server{
		Addr:        ln.Addr().String(),
		Handler:     mux,
		ReadTimeout: time.Minute,
		TLSConfig:   tlsCfg,
	}
	g.Go(func() error {
		return server.Serve(ln)
	})
	<-gCtx.Done()
	_ = server.Close()
	err = g.Wait()
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.Wrap(err, "failed to serve remote controller")
}

// submitWorkloadSetupTXs writes the workload setup TXs to the channel, and waits for them to be committed.
func (c *Client) submitWorkloadSetupTXs(ctx context.Context, txs chan *servicepb.LoadGenTx) error {
	defer close(txs)

	workloadSetupTXs, err := makeWorkloadSetupTXs(c.conf, &c.resources)
	if err != nil {
		return err
	}

	txChan := channel.NewWriter(ctx, txs)
	curProgress := c.adapter.Progress()
	for _, tx := range workloadSetupTXs {
		logger.Infof("Submitting TX [%s]. Progress: %d", tx.Id, curProgress)
		if !txChan.Write(tx) {
			return errors.New("context ended before submitting TX")
		}

		for lastProgress := curProgress; curProgress == lastProgress; curProgress = c.adapter.Progress() {
			select {
			case <-ctx.Done():
				return errors.New("context ended before acknowledging TX")
			case <-time.Tick(time.Second):
			}
		}
		logger.Infof("TX [%s] submitted. Progress: %d", tx.Id, curProgress)
	}
	return nil
}

func makeWorkloadSetupTXs(config *ClientConfig, res *adapters.ClientResources) ([]*servicepb.LoadGenTx, error) {
	workloadSetupTXs := make([]*servicepb.LoadGenTx, 0, 2)
	if config.Generate.Config {
		configTX, err := workload.CreateConfigTxFromConfigBlock(res.ConfigBlock)
		if err != nil {
			return nil, err
		}
		workloadSetupTXs = append(workloadSetupTXs, configTX)
	}
	if config.Generate.Namespaces {
		metaNsTX, err := workload.CreateLoadGenNamespacesTX(&config.LoadProfile.Policy)
		if err != nil {
			return nil, err
		}
		workloadSetupTXs = append(workloadSetupTXs, metaNsTX)
	}
	return workloadSetupTXs, nil
}
