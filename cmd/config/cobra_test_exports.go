/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

// CommandTest is a struct that represents a CMD unit test.
type CommandTest struct {
	Name              string
	Args              []string
	CmdLoggerOutputs  []string
	CmdStdOutput      string
	Err               error
	UseConfigTemplate string
	System            SystemConfig
}

// StartDefaultSystem starts a system with mocks for CMD testing.
func StartDefaultSystem(t *testing.T) SystemConfig {
	t.Helper()
	_, verifier := mock.StartMockSVService(t, 1)
	_, vc := mock.StartMockVCService(t, 1)
	_, orderer := mock.StartMockOrderingServices(t, &mock.OrdererConfig{NumService: 1})
	_, coordinator := mock.StartMockCoordinatorService(t)
	conn := dbtest.PrepareTestEnv(t)
	server := connection.NewLocalHostServer()
	listen, err := server.Listener()
	require.NoError(t, err)
	connection.CloseConnectionsLog(listen)
	return SystemConfig{
		ServiceEndpoints: ServiceEndpoints{
			Server: &server.Endpoint,
		},
		Endpoints: SystemEndpoints{
			Verifier:    []ServiceEndpoints{{Server: &verifier.Configs[0].Endpoint}},
			VCService:   []ServiceEndpoints{{Server: &vc.Configs[0].Endpoint}},
			Orderer:     []ServiceEndpoints{{Server: &orderer.Configs[0].Endpoint}},
			Coordinator: ServiceEndpoints{Server: &coordinator.Configs[0].Endpoint},
		},
		DB: DatabaseConfig{
			Name:        conn.Database,
			LoadBalance: false,
			Endpoints:   conn.Endpoints,
		},
		Policy: &workload.PolicyProfile{
			ChannelID: "channel1",
		},
		LedgerPath: t.TempDir(),
	}
}

// UnitTestRunner is a function that runs a unit test with the requested parameters passed in CommandTest structure.
func UnitTestRunner(
	t *testing.T,
	cmd *cobra.Command,
	test CommandTest,
) {
	t.Helper()
	// Set new logger with the config setup.
	loggerPath := filepath.Clean(path.Join(t.TempDir(), "logger-output.txt"))
	logConfig := &logging.Config{
		Enabled:     true,
		Level:       logging.Debug,
		Caller:      true,
		Development: true,
		Output:      loggerPath,
	}
	logging.SetupWithConfig(logConfig)
	test.System.Logging = logConfig

	args := test.Args
	if test.UseConfigTemplate != "" {
		configPath := CreateTempConfigFromTemplate(t, test.UseConfigTemplate, &test.System)
		args = append(args, "--config", configPath)
	}
	cmd.SetArgs(args)

	// Creating new buffers for the cmd stdout and stderr and redirect the CMD output.
	var cmdStdOut bytes.Buffer
	var cmdStdErr bytes.Buffer
	cmd.SetOut(&cmdStdOut)
	cmd.SetErr(&cmdStdErr)

	wg := &sync.WaitGroup{}
	t.Cleanup(func() {
		t.Log("Waiting for command to finish")
		defer t.Log("Command finished")
		wg.Wait()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	t.Cleanup(cancel)

	wg.Add(1)
	go func() {
		t.Log("Starting command")
		defer t.Log("Command exited")
		defer wg.Done()
		_, err := cmd.ExecuteContextC(ctx)
		err = connection.FilterStreamRPCError(err)
		assert.Equal(t, test.Err, err)
	}()

	assert.Eventually(t, func() bool {
		return len(getMissing(test, &cmdStdOut, loggerPath)) == 0
	}, 10*time.Minute, 500*time.Millisecond)

	t.Log("Stopping command, and waiting for finish")
	cancel()
	wg.Wait()
	if !t.Failed() {
		return
	}

	t.Log("Test finished with error")
	t.Log("STD OUT:\n", cmdStdOut.String())
	t.Log("STD ERR:\n", cmdStdErr.String())
	logOut, err := os.ReadFile(loggerPath)
	if err == nil {
		t.Log("LOG:\n", string(logOut))
	}
	for _, m := range getMissing(test, &cmdStdOut, loggerPath) {
		t.Logf("Missing: %s", m)
	}
}

func getMissing(test CommandTest, cmdStdOut *bytes.Buffer, loggerPath string) (missing []string) {
	if test.CmdStdOutput != "" && !strings.Contains(cmdStdOut.String(), test.CmdStdOutput) {
		missing = append(missing, test.CmdStdOutput)
	}
	if len(test.CmdLoggerOutputs) == 0 {
		return missing
	}
	logOut, err := os.ReadFile(loggerPath)
	if err != nil {
		return append(missing, loggerPath)
	}
	for _, loggerLine := range test.CmdLoggerOutputs {
		if !strings.Contains(string(logOut), loggerLine) {
			missing = append(missing, loggerLine)
		}
	}
	return missing
}
