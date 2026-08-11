/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	// ProcessWithConfig holds the process and the corresponding configuration.
	ProcessWithConfig struct {
		params         CmdParameters
		process        *test.Process
		configFilePath string

		// thisService is this process's own endpoint config. writeConfig patches it into the
		// full system config as ThisService when rendering the process's config file, once every
		// service endpoint is known.
		thisService config.ServiceConfig

		// reservations hold the ports this process binds. While the subprocess is not running,
		// the runner keeps these ports reserved (see the port-reservation section below) so a
		// parallel test cannot grab them; the reservation is released to the subprocess when it
		// starts and re-taken when it stops. Accessed only from the test goroutine.
		reservations reservationSet
	}

	// CmdParameters holds the parameters for a command.
	CmdParameters struct {
		Name     string
		Bin      string
		Args     []string
		Template string
	}

	// serviceParams carries the credentials factory and TLS mode used to build each service
	// endpoint. It is passed to allocateService (via newProcess); it holds no state between
	// allocations, since each allocated port is owned by the process it is handed to (see newProcess).
	serviceParams struct {
		credFactory *test.CredentialsFactory
		tlsMode     string
	}

	// reservationSet is the set of port reservations a single process owns (its gRPC and HTTP ports).
	reservationSet []portReservation

	// portReservation holds one endpoint's port open by keeping a bound listener on it. The
	// reservation is "held" (the runner owns the port) when listener != nil, and "released" (the
	// subprocess owns the port) when listener == nil. endpoint persists across a release so the
	// port can be re-taken later.
	portReservation struct {
		endpoint *connection.Endpoint
		listener net.Listener
	}
)

const (
	committerCMD = "committer"
	loadgenCMD   = "loadgen"
	mockCMD      = "mock"
)

// portReserveTimeout bounds the retry budget reservationSet.take uses when rebinding a
// just-freed port.
const portReserveTimeout = 5 * time.Second

var (
	cmdOrderer = CmdParameters{
		Name:     "orderer",
		Bin:      mockCMD,
		Args:     []string{"start", "orderer"},
		Template: config.TemplateMockOrderer,
	}
	cmdVerifier = CmdParameters{
		Name:     "verifier",
		Bin:      committerCMD,
		Args:     []string{"start", "verifier"},
		Template: config.TemplateVerifier,
	}
	cmdVC = CmdParameters{
		Name:     "vc",
		Bin:      committerCMD,
		Args:     []string{"start", "vc"},
		Template: config.TemplateVC,
	}
	cmdCoordinator = CmdParameters{
		Name:     "coordinator",
		Bin:      committerCMD,
		Args:     []string{"start", "coordinator"},
		Template: config.TemplateCoordinator,
	}
	cmdSidecar = CmdParameters{
		Name:     "sidecar",
		Bin:      committerCMD,
		Args:     []string{"start", "sidecar"},
		Template: config.TemplateSidecar,
	}
	cmdQuery = CmdParameters{
		Name:     "query",
		Bin:      committerCMD,
		Args:     []string{"start", "query"},
		Template: config.TemplateQueryService,
	}
	cmdLoadGen = CmdParameters{
		Name: "loadgen",
		Bin:  loadgenCMD,
		Args: []string{"start"},
		// Template is left unset: the primary load generator's template depends on the service flags
		// passed to Start, so startLoadGen sets it before rendering the config.
	}
	cmdLoadGenDist = CmdParameters{
		Name:     "dist-loadgen",
		Bin:      loadgenCMD,
		Args:     []string{"start"},
		Template: config.TemplateLoadGenDistributedLoadGenClient,
	}
)

// newProcess allocates the gRPC and HTTP ports for a service and returns the process that owns the
// reservations (holding them against parallel tests until the subprocess binds them and re-holding
// them whenever it is stopped) together with its allocated endpoint config, which the caller
// publishes into the system config so the templates can reference this service. The process's own
// config file is written later by writeConfig, once every service endpoint is known.
func newProcess(
	t *testing.T, params serviceParams, cmdParams CmdParameters,
) (*ProcessWithConfig, config.ServiceConfig) {
	t.Helper()
	thisService, reservations := allocateService(t, params)
	return buildProcess(t, cmdParams, thisService, reservations), thisService
}

// newExternalProcess returns a process whose ports are reserved outside allocateService, so it owns
// no reservations: the mock orderer's ports are reserved by OrdererEnv, and the distributed loadgen
// client binds its own ephemeral port. The config file is written later by writeConfig.
func newExternalProcess(
	t *testing.T, cmdParams CmdParameters, thisService config.ServiceConfig,
) *ProcessWithConfig {
	t.Helper()
	return buildProcess(t, cmdParams, thisService, nil)
}

// buildProcess constructs a ProcessWithConfig for a service with the given endpoint config and the
// port reservations it owns (nil when reserved elsewhere; see newExternalProcess), and registers its
// cleanup. The config file is written later by writeConfig, once every service endpoint is known.
func buildProcess(
	t *testing.T, cmdParams CmdParameters, thisService config.ServiceConfig, reservations reservationSet,
) *ProcessWithConfig {
	t.Helper()
	proc := &ProcessWithConfig{
		params:       cmdParams,
		thisService:  thisService,
		reservations: reservations,
	}
	t.Cleanup(func() {
		proc.close(t)
	})
	return proc
}

// writeConfig renders this process's config file from the full system config with its own endpoint
// (thisService) patched in as ThisService. It must be called only once every service endpoint is
// allocated, because a template may reference any service's endpoint (e.g. the coordinator dials the
// verifiers and VC services).
func (p *ProcessWithConfig) writeConfig(t *testing.T, conf *config.SystemConfig) {
	t.Helper()
	s := *conf
	s.ThisService = p.thisService
	p.configFilePath = config.CreateTempConfigFromTemplate(t, p.params.Template, &s)
}

// requireRunning fails immediately if the process has already exited. It takes a [test.TestingT] so
// a polling condition can pass its [assert.CollectT] and fail only that tick.
func (p *ProcessWithConfig) requireRunning(t test.TestingT) {
	t.Helper()
	if p == nil || p.process == nil {
		return
	}

	select {
	case err := <-p.process.Wait():
		require.Failf(t, "process exited unexpectedly", "[%s]: %v", p.params.Name, err)
	default:
	}
}

// Restart stops the process if it is running and then starts it.
func (p *ProcessWithConfig) Restart(t *testing.T) {
	t.Helper()
	// Stop re-reserves the ports the moment the old subprocess frees them, closing the window in
	// which a parallel test could grab a just-freed port before the new subprocess binds it
	// (mirrors OrdererEnv.StopServers + ReserveListeners). On the first start the reservation is
	// still held, so re-reserving is a no-op.
	p.Stop(t)

	cmdPath := path.Join("bin", p.params.Bin)
	c := exec.Command(cmdPath, append(p.params.Args, "--config", p.configFilePath)...)
	dir, err := os.Getwd()
	require.NoError(t, err)
	c.Dir = path.Clean(path.Join(dir, "../.."))
	process, err := test.NewProcess(c, p.params.Name)
	require.NoError(t, err)
	p.process = process

	// Hand the ports to the subprocess: release our reservation so the subprocess,
	// which is already retrying to bind (ListenRetryExecute), can claim them.
	p.reservations.release()
}

// Stop stops the running process.
func (p *ProcessWithConfig) Stop(t *testing.T) {
	t.Helper()
	p.killProcess(t)
	// The subprocess released the ports as it exited. Re-reserve them so a parallel test
	// cannot grab them while this service is down (e.g. crash tests), until the next Restart
	// hands them to a new subprocess.
	p.reservations.take(t)
}

// killProcess signals the running subprocess to stop and waits for it to exit.
func (p *ProcessWithConfig) killProcess(t *testing.T) {
	t.Helper()
	if p.process == nil {
		return
	}
	p.process.Signal(os.Kill)
	select {
	case err := <-p.process.Wait():
		t.Logf("Process [%s] exited by request with error: %v", p.params.Name, err)
	case <-time.After(30 * time.Second):
		t.Errorf("Process [%s] did not terminate after 30 seconds", p.params.Name)
	}
	p.process = nil
}

// close terminates the subprocess and releases the reserved ports.
// It is registered as a test cleanup.
func (p *ProcessWithConfig) close(t *testing.T) {
	t.Helper()
	p.killProcess(t)
	p.reservations.release()
}

// Port reservation. Parallel integration tests share the host's ephemeral port range, so a port that
// has been allocated but not yet bound by its owning subprocess can be stolen by another test. To
// prevent that, each allocated port is kept reserved (a listener held open on it) from allocation
// until the subprocess that will use it has bound it, and re-reserved whenever that subprocess is
// stopped. allocateService binds a service's ports and hands the reservations straight to the process
// that owns them (see newProcess); a process releases them when it hands the ports to its subprocess
// (Restart) and re-takes them while the subprocess is down (Stop); the reservations are closed by the
// process's test cleanup (close).

// allocateService binds a gRPC and an HTTP port for a service and issues the endpoint config
// (endpoints plus TLS credentials) together with the reservations the owning process must hold
// until its subprocess binds them (see newProcess). Each port is kept reserved (a listener held
// open) so no parallel test can grab it in between.
func allocateService(t *testing.T, params serviceParams) (config.ServiceConfig, reservationSet) {
	t.Helper()
	grpcEndpoint, grpcRes := allocateEndpoint(t)
	httpEndpoint, httpRes := allocateEndpoint(t)
	grpcTLS, _ := params.credFactory.CreateServerCredentials(t, params.tlsMode, grpcEndpoint.Host)
	httpTLS, _ := params.credFactory.CreateServerCredentials(t, params.tlsMode, httpEndpoint.Host)
	return config.ServiceConfig{
		GrpcEndpoint: grpcEndpoint,
		HTTPEndpoint: httpEndpoint,
		GrpcTLS:      grpcTLS,
		HTTPTLS:      httpTLS,
	}, reservationSet{grpcRes, httpRes}
}

// allocateEndpoint binds an ephemeral port and returns the endpoint together with a reservation
// holding its listener open, so the port stays reserved until the owning subprocess binds it.
func allocateEndpoint(t *testing.T) (*connection.Endpoint, portReservation) {
	t.Helper()
	s := test.NewLocalHostServer(test.InsecureTLSConfig)
	listener, err := s.Listener(t.Context())
	require.NoError(t, err)
	return &s.Endpoint, portReservation{endpoint: &s.Endpoint, listener: listener}
}

// release hands every reserved port to the subprocess by closing the listeners we hold. The
// subprocess is already retrying to bind them (serve.ListenRetryExecute). Idempotent.
func (s reservationSet) release() {
	for i := range s {
		listener := s[i].listener
		s[i].listener = nil
		connection.CloseConnectionsLog(listener)
	}
}

// take re-binds every released port and holds it, so a parallel test cannot claim it while the
// subprocess is down. It is bounded by a single portReserveTimeout budget shared across the set,
// and is best-effort per port: a port that fails to bind is left free for the next subprocess.
func (s reservationSet) take(t *testing.T) {
	t.Helper()
	if len(s) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), portReserveTimeout)
	defer cancel()
	for i := range s {
		if s[i].listener != nil {
			continue
		}
		var listener net.Listener
		err := serve.ListenRetryExecute(ctx, func() error {
			var listenErr error
			listener, listenErr = net.Listen("tcp", s[i].endpoint.Address())
			return listenErr
		})
		if err != nil {
			// Ignored on purpose: re-reserving is best-effort, not a hard guarantee. killProcess
			// already waited for the previous subprocess to exit, so the port is normally free and
			// binds on the first attempt; the bounded retry only rides out a transient conflict. If
			// it still fails, we leave the port free — the next subprocess retries binding it itself
			// via serve.ListenRetryExecute, which is the actual backstop against the port race.
			continue
		}
		s[i].listener = listener
	}
}
