/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/go-playground/validator/v10"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/loadgen"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/service/coordinator"
	"github.com/hyperledger/fabric-x-committer/service/query"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

var (
	logger   = flogging.MustGetLogger("config-reader")
	validate = validator.New()
)

// ReadSidecarYamlAndSetupLogging reading the YAML config file of the sidecar.
func ReadSidecarYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*sidecar.Config, *serve.Config, error) {
	return ReadYamlAndSetupLogging[sidecar.Config](v, configPath, "SIDECAR")
}

// ReadCoordinatorYamlAndSetupLogging reading the YAML config file of the coordinator.
func ReadCoordinatorYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*coordinator.Config, *serve.Config, error) {
	return ReadYamlAndSetupLogging[coordinator.Config](v, configPath, "COORDINATOR")
}

// ReadVCYamlAndSetupLogging reading the YAML config file of the VC.
func ReadVCYamlAndSetupLogging(v *viper.Viper, configPath string) (*vc.Config, *serve.Config, error) {
	return ReadYamlAndSetupLogging[vc.Config](v, configPath, "VC")
}

// ReadVerifierYamlAndSetupLogging reading the YAML config file of the verifier.
func ReadVerifierYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*verifier.Config, *serve.Config, error) {
	return ReadYamlAndSetupLogging[verifier.Config](v, configPath, "VERIFIER")
}

// ReadQueryYamlAndSetupLogging reading the YAML config file of the query service.
func ReadQueryYamlAndSetupLogging(v *viper.Viper, configPath string) (*query.Config, *serve.Config, error) {
	return ReadYamlAndSetupLogging[query.Config](v, configPath, "QUERY")
}

// ReadMockOrdererYamlAndSetupLogging reading the YAML config file of the mock ordering service.
func ReadMockOrdererYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*mock.OrdererConfig, *serve.Config, error) {
	return ReadYamlAndSetupLogging[mock.OrdererConfig](v, configPath, "ORDERER")
}

// ReadLoadGenYamlAndSetupLogging reading the YAML config file of the load generator.
func ReadLoadGenYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*loadgen.ClientConfig, *serve.Config, error) {
	return ReadYamlAndSetupLogging[loadgen.ClientConfig](v, configPath, "LOADGEN")
}

// ReadYamlAndSetupLogging reading the YAML config file of a service.
// It parses the server configuration separately from the service-specific configuration,
// following the same pattern as logging configuration.
func ReadYamlAndSetupLogging[T any](v *viper.Viper, configPath, servicePrefix string) (*T, *serve.Config, error) {
	if configPath == "" {
		return nil, nil, errors.New("--config flag must be set")
	}
	content, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to read config file: %s", configPath)
	}
	if err = readYamlConfigsFromIO(v, bytes.NewReader(content)); err != nil {
		return nil, nil, err
	}
	setupEnv(v, servicePrefix)

	loggingWrapper := struct {
		Logging *flogging.Config `mapstructure:"logging"`
	}{}
	if err = unmarshal(v, &loggingWrapper); err != nil {
		return nil, nil, err
	}
	flogging.Init(*loggingWrapper.Logging)

	// Parse server configuration separately (similar to logging)
	serverConfig := &serve.Config{}
	if err = unmarshal(v, serverConfig); err != nil {
		return nil, nil, err
	}

	// Parse service-specific configuration
	c := new(T)
	if err = unmarshal(v, c); err != nil {
		return nil, nil, err
	}

	return c, serverConfig, nil
}

// unmarshal populate a config object and validate it.
func unmarshal(v *viper.Viper, c any) error {
	defer logger.Debugf("Decoded config: %s", &utils.LazyJSON{O: c})
	if err := v.Unmarshal(c, decoderHook()); err != nil {
		return errors.Wrap(err, "error decoding config")
	}
	return errors.Wrap(validate.Struct(c), "error validating config")
}

// setupEnv enables setting configuration via environment variables.
// E.g. SC_LOGGING_ENABLED=false, but not SC_VERBOSE=false (does not work with aliases).
func setupEnv(v *viper.Viper, servicePrefix string) {
	v.SetEnvPrefix("SC_" + strings.ToUpper(servicePrefix))
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
}

// readYamlConfigsFromIO reads configurations from IO.
func readYamlConfigsFromIO(v *viper.Viper, in io.Reader) error {
	v.SetConfigType("yaml")
	return errors.Wrap(v.ReadConfig(in), "failed to read config")
}
