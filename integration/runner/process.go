/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
)

type (
	// ProcessWithConfig holds the ifrit process and the corresponding configuration.
	ProcessWithConfig struct {
		params         CmdParameters
		process        ifrit.Process
		configFilePath string
	}

	// CmdParameters holds the parameters for a command.
	CmdParameters struct {
		Name     string
		Bin      string
		Arg      string
		Template string
	}
)

const (
	committerCMD = "committer"
	loadgenCMD   = "loadgen"
	mockCMD      = "mock"
)

var (
	cmdOrderer = CmdParameters{
		Name:     "orderer",
		Bin:      mockCMD,
		Arg:      "start-orderer",
		Template: config.TemplateMockOrderer,
	}
	cmdVerifier = CmdParameters{
		Name:     "verifier",
		Bin:      committerCMD,
		Arg:      "start-verifier",
		Template: config.TemplateVerifier,
	}
	cmdVC = CmdParameters{
		Name:     "vc",
		Bin:      committerCMD,
		Arg:      "start-vc",
		Template: config.TemplateVC,
	}
	cmdCoordinator = CmdParameters{
		Name:     "coordinator",
		Bin:      committerCMD,
		Arg:      "start-coordinator",
		Template: config.TemplateCoordinator,
	}
	cmdSidecar = CmdParameters{
		Name:     "sidecar",
		Bin:      committerCMD,
		Arg:      "start-sidecar",
		Template: config.TemplateSidecar,
	}
	cmdQuery = CmdParameters{
		Name:     "query",
		Bin:      committerCMD,
		Arg:      "start-query",
		Template: config.TemplateQueryService,
	}
	cmdLoadGen = CmdParameters{
		Name: "loadgen",
		Bin:  loadgenCMD,
		Arg:  "start",
	}
)

func newProcess(
	t *testing.T,
	cmdParams CmdParameters,
	conf *config.SystemConfig,
	thisService config.ServiceConfig,
) *ProcessWithConfig {
	t.Helper()
	s := *conf
	s.ThisService = thisService
	configFilePath := config.CreateTempConfigFromTemplate(t, cmdParams.Template, &s)
	p := &ProcessWithConfig{
		params:         cmdParams,
		configFilePath: configFilePath,
	}
	t.Cleanup(func() {
		p.Stop(t)
	})
	return p
}

// Stop stops the running process.
func (p *ProcessWithConfig) Stop(t *testing.T) {
	t.Helper()
	if p.process == nil {
		return
	}
	p.process.Signal(os.Kill)
	select {
	case <-p.process.Wait():
	case <-time.After(30 * time.Second):
		t.Errorf("Process [%s] did not terminate after 30 seconds", p.params.Name)
	}
}

// Restart stops the process if it is running and then starts it.
func (p *ProcessWithConfig) Restart(t *testing.T) {
	t.Helper()
	p.Stop(t)
	cmdPath := path.Join("bin", p.params.Bin)
	c := exec.Command(cmdPath, p.params.Arg, "--config", p.configFilePath)
	dir, err := os.Getwd()
	require.NoError(t, err)
	c.Dir = path.Clean(path.Join(dir, "../.."))
	p.process = Run(c, p.params.Name, "")
}

// Run executes the specified command and returns the corresponding process.
// It is important to note that the underlying invocation function (Invoke)
// returns only when either process.Ready or process.Wait has been read.
// Consequently, the caller only needs to read process.Wait to wait for the
// process to complete and capture any errors that may have occurred during execution.
//
//nolint:ireturn
func Run(cmd *exec.Cmd, name, startCheck string) ifrit.Process {
	return ifrit.Invoke(ginkgomon.New(ginkgomon.Config{
		Command:           cmd,
		Name:              name,
		AnsiColorCode:     "",
		StartCheck:        startCheck,
		StartCheckTimeout: 0,
		Cleanup: func() {
		},
	}))
}
