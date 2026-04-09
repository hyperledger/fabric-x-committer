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

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/test"
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
		Args     []string
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
	p.process = nil
}

// requireRunning fails the test immediately if the process has already exited.
func (p *ProcessWithConfig) requireRunning(t *testing.T) {
	t.Helper()
	if p == nil || p.process == nil {
		return
	}

	select {
	case err := <-p.process.Wait():
		t.Fatalf("Process [%s] exited unexpectedly: %v", p.params.Name, err)
	default:
	}
}

// Restart stops the process if it is running and then starts it.
func (p *ProcessWithConfig) Restart(t *testing.T) {
	t.Helper()
	p.Stop(t)
	cmdPath := path.Join("bin", p.params.Bin)
	c := exec.Command(cmdPath, append(p.params.Args, "--config", p.configFilePath)...)
	dir, err := os.Getwd()
	require.NoError(t, err)
	c.Dir = path.Clean(path.Join(dir, "../.."))
	p.process = test.Run(c, p.params.Name, "")
}
