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
	"reflect"
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
	"github.com/hyperledger/fabric-x-committer/utils/db"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

type (
	loggingConfig struct {
		Logging flogging.Config `mapstructure:"logging"`
	}

	databaseInitConfig struct {
		Database *db.Config `mapstructure:"database" validate:"required"`
	}
)

var (
	logger   = flogging.MustGetLogger("config-reader")
	validate = validator.New()
)

// ReadSidecarYamlAndSetupLogging reading the YAML config file of the sidecar.
func ReadSidecarYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*sidecar.Config, *serve.Config, error) {
	return readYamlAndSetupLogging[sidecar.Config](v, configPath)
}

// ReadCoordinatorYamlAndSetupLogging reading the YAML config file of the coordinator.
func ReadCoordinatorYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*coordinator.Config, *serve.Config, error) {
	return readYamlAndSetupLogging[coordinator.Config](v, configPath)
}

// ReadVCYamlAndSetupLogging reading the YAML config file of the VC.
func ReadVCYamlAndSetupLogging(v *viper.Viper, configPath string) (*vc.Config, *serve.Config, error) {
	return readYamlAndSetupLogging[vc.Config](v, configPath)
}

// ReadVerifierYamlAndSetupLogging reading the YAML config file of the verifier.
func ReadVerifierYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*verifier.Config, *serve.Config, error) {
	return readYamlAndSetupLogging[verifier.Config](v, configPath)
}

// ReadQueryYamlAndSetupLogging reading the YAML config file of the query service.
func ReadQueryYamlAndSetupLogging(v *viper.Viper, configPath string) (*query.Config, *serve.Config, error) {
	return readYamlAndSetupLogging[query.Config](v, configPath)
}

// ReadMockOrdererYamlAndSetupLogging reading the YAML config file of the mock ordering service.
func ReadMockOrdererYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*mock.OrdererConfig, *serve.Config, error) {
	return readYamlAndSetupLogging[mock.OrdererConfig](v, configPath)
}

// ReadLoadGenYamlAndSetupLogging reading the YAML config file of the load generator.
func ReadLoadGenYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*loadgen.ClientConfig, *serve.Config, error) {
	return readYamlAndSetupLogging[loadgen.ClientConfig](v, configPath)
}

// ReadDBInitYamlAndSetupLogging reading the YAML config file for database initialization.
func ReadDBInitYamlAndSetupLogging(
	v *viper.Viper, configPath string,
) (*db.Config, *serve.Config, error) {
	cfg, srvCfg, err := readYamlAndSetupLogging[databaseInitConfig](v, configPath)
	if err != nil {
		return nil, nil, err
	}
	return cfg.Database, srvCfg, nil
}

// readYamlAndSetupLogging reading the YAML config file of a service.
// It parses the server configuration separately from the service-specific configuration,
// following the same pattern as logging configuration.
func readYamlAndSetupLogging[T any](v *viper.Viper, configPath string) (*T, *serve.Config, error) {
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

	// Check if the YAML is overridden by an environment variable.
	yamlEnvKey := v.GetEnvPrefix() + "_YAML"
	if envValue, ok := os.LookupEnv(yamlEnvKey); ok && envValue != "" {
		logger.Debugf("Overridding config YAML from env var '%s'", yamlEnvKey)
		if err = readYamlConfigsFromIO(v, strings.NewReader(envValue)); err != nil {
			return nil, nil, err
		}
	}

	// Parse logging, server configuration, and target config.
	loggingConf := new(loggingConfig)
	serverConf := new(serve.Config)
	targetConf := new(T)
	if err = unmarshal(v, loggingConf, serverConf, targetConf); err != nil {
		return nil, nil, err
	}
	flogging.Init(loggingConf.Logging)

	return targetConf, serverConf, nil
}

// unmarshal populate multiple config objects and validates them.
// It automatically set configuration via environment variables.
func unmarshal(v *viper.Viper, items ...any) error {
	for _, c := range items {
		setEnvVars(v, reflect.TypeOf(c))
	}

	decoders := decoderHook()
	for _, c := range items {
		if err := v.Unmarshal(c, decoders); err != nil {
			return errors.Wrap(err, "error decoding config")
		}
		logger.Debugf("Decoded config: %s", &utils.LazyJSON{O: c})
		if err := validate.Struct(c); err != nil {
			return errors.Wrap(err, "error validating config")
		}
	}
	return nil
}

// readYamlConfigsFromIO reads configurations from IO.
func readYamlConfigsFromIO(v *viper.Viper, in io.Reader) error {
	v.SetConfigType("yaml")
	return errors.Wrap(v.ReadConfig(in), "failed to read config")
}
