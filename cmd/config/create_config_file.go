/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	sprig "github.com/go-task/slim-sprig/v3"
	"github.com/google/uuid"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/internaltools/configtxgen"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

type (
	// SystemConfig represents the configuration of the one of the committer's components.
	SystemConfig struct {
		// Instance endpoints.
		ServiceEndpoints ServiceEndpoints

		// System's resources.
		Endpoints SystemEndpoints
		DB        DatabaseConfig

		// Per service configurations.
		BlockSize         uint64                  // orderer, loadgen
		BlockTimeout      time.Duration           // orderer
		ConfigBlockPath   string                  // orderer, sidecar, loadgen
		LedgerPath        string                  // sidecar
		ChannelID         string                  // sidecar, loadgen
		Policy            *workload.PolicyProfile // loadgen
		LoadGenBlockLimit uint64                  // loadgen
		LoadGenTXLimit    uint64                  // loadgen
		LoadGenWorkers    uint64                  // loadgen
		Logging           *logging.Config         // for all
	}

	// SystemEndpoints represents the endpoints of the system.
	SystemEndpoints struct {
		Verifier    []ServiceEndpoints
		VCService   []ServiceEndpoints
		Orderer     []ServiceEndpoints
		Coordinator ServiceEndpoints
		Sidecar     ServiceEndpoints
		Query       ServiceEndpoints
		LoadGen     ServiceEndpoints
	}

	// ServiceEndpoints stores the server and metrics endpoints for a service.
	ServiceEndpoints struct {
		Server  *connection.Endpoint
		Metrics *connection.Endpoint
	}

	// DatabaseConfig represents the used DB.
	DatabaseConfig struct {
		Name        string
		LoadBalance bool
		Endpoints   []*connection.Endpoint
	}

	// ConfigBlock represents the configuration of the config block.
	ConfigBlock = workload.ConfigBlock //nolint:revive
)

// Config templates.
var (
	//go:embed templates/coordinator.yaml
	TemplateCoordinator string
	//go:embed templates/mockorderingservice.yaml
	TemplateMockOrderer string
	//go:embed templates/queryexecutor.yaml
	TemplateQueryService string
	//go:embed templates/sidecar.yaml
	TemplateSidecar string
	//go:embed templates/validatorpersister.yaml
	TemplateVC string
	//go:embed templates/signatureverifier.yaml
	TemplateVerifier string

	TemplateLoadGenOnlyOrderer              = templateLoadGenOnlyOrdererClient + templateLoadGenCommon
	TemplateLoadGenOrderer                  = templateLoadGenOrdererClient + templateLoadGenCommon
	TemplateLoadGenCommitter                = templateLoadGenCommitterClient + templateLoadGenCommon
	TemplateLoadGenCoordinator              = templateLoadGenCoordinatorClient + templateLoadGenCommon
	TemplateLoadGenVC                       = templateLoadGenVCClient + templateLoadGenCommon
	TemplateLoadGenVerifier                 = templateLoadGenVerifierClient + templateLoadGenCommon
	TemplateLoadGenDistributedLoadGenClient = templateLoadGenDistributedLoadGenClient + templateLoadGenCommon

	//go:embed templates/loadgen_common.yaml
	templateLoadGenCommon string
	//go:embed templates/loadgen_client_only_orderer.yaml
	templateLoadGenOnlyOrdererClient string
	//go:embed templates/loadgen_client_orderer.yaml
	templateLoadGenOrdererClient string
	//go:embed templates/loadgen_client_sidecar.yaml
	templateLoadGenCommitterClient string
	//go:embed templates/loadgen_client_coordinator.yaml
	templateLoadGenCoordinatorClient string
	//go:embed templates/loadgen_client_vc.yaml
	templateLoadGenVCClient string
	//go:embed templates/loadgen_client_verifier.yaml
	templateLoadGenVerifierClient string
	//go:embed templates/loadgen_client_distributed_loadgen.yaml
	templateLoadGenDistributedLoadGenClient string
)

// CreateConfigFromTemplate creates a config file using template yaml and writes it to the outputPath.
func CreateConfigFromTemplate(t *testing.T, templateString, outputPath string, conf *SystemConfig) {
	t.Helper()
	tmpl := template.New("").Funcs(sprig.FuncMap())
	tmpl, err := tmpl.Parse(templateString)
	require.NoError(t, err)

	var renderedConfig bytes.Buffer
	require.NoError(t, tmpl.Execute(&renderedConfig, conf))
	err = os.WriteFile(outputPath, renderedConfig.Bytes(), 0o644)
	require.NoError(t, err)
}

// CreateTempConfigFromTemplate creates a temporary config file and returning the temporary output config path.
func CreateTempConfigFromTemplate(t *testing.T, cmdTemplate string, conf *SystemConfig) string {
	t.Helper()
	outputConfigFilePath := path.Join(t.TempDir(), fmt.Sprintf("config-%s.yaml", uuid.NewString()))
	CreateConfigFromTemplate(t, cmdTemplate, outputConfigFilePath, conf)
	return outputConfigFilePath
}

// CreateConfigBlock create and writes a config block to file.
func CreateConfigBlock(t *testing.T, conf *ConfigBlock) string {
	t.Helper()
	block, err := workload.CreateDefaultConfigBlock(conf)
	require.NoError(t, err)
	return WriteConfigBlock(t, block)
}

// WriteConfigBlock writes a config block to file.
func WriteConfigBlock(t *testing.T, block *common.Block) string {
	t.Helper()
	blockDir := t.TempDir()
	configBlockPath := filepath.Join(blockDir, "config.block")
	require.NoError(t, configtxgen.WriteOutputBlock(block, configBlockPath))
	return configBlockPath
}

// WithEndpoint creates a new SystemConfig with a modified ServerEndpoint and MetricsEndpoint.
func (c *SystemConfig) WithEndpoint(e ServiceEndpoints) *SystemConfig {
	s := *c
	s.ServiceEndpoints = e
	return &s
}
