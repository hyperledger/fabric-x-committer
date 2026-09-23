//go:build integration

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package migration_test

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const fabricSourceVersion = "3.1.5"

func captureFabricSnapshots(t *testing.T, repository, stateDB string, channels ...string) map[string]string {
	t.Helper()
	return newFabricNetwork(t, repository, stateDB).capture(channels...)
}

type fabricNetwork struct {
	t       *testing.T
	root    string
	home    string
	samples string
	version string
	stateDB string
}

func newFabricNetwork(t *testing.T, repository, stateDB string) *fabricNetwork {
	t.Helper()
	root := t.TempDir()
	home := os.Getenv("FABRIC_SOURCE_HOME")
	if home == "" {
		home = filepath.Join(repository, ".cache", "fabric", fabricSourceVersion)
	}
	ensureFabricRelease(t, home, fabricSourceVersion)
	samples := ensureFabricSamples(t, repository)
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	setup := filepath.Join(filepath.Dir(sourceFile), "testdata")
	for _, name := range []string{"compose.yaml", "configtx.yaml", "crypto-config.yaml", "collections.json"} {
		content, err := os.ReadFile(filepath.Join(setup, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), content, 0o600))
	}
	fabric := &fabricNetwork{t: t, root: root, home: home, samples: samples, version: fabricSourceVersion, stateDB: stateDB}
	fabric.run(fabric.bin("cryptogen"), "generate", "--config="+filepath.Join(root, "crypto-config.yaml"), "--output="+fabric.crypto())
	t.Cleanup(func() { fabric.compose("down", "--volumes", "--remove-orphans") })
	return fabric
}

func (f *fabricNetwork) capture(channels ...string) map[string]string {
	f.t.Helper()
	for _, channel := range channels {
		command := f.command(f.bin("configtxgen"), "-profile", "MigrationChannel", "-channelID", channel, "-outputBlock", filepath.Join(f.root, channel+".block"))
		command.Env = append(command.Env, "FABRIC_CFG_PATH="+f.root)
		f.runCommand(command)
	}
	f.compose("up", "-d")
	require.Eventually(f.t, func() bool {
		return f.peerCommand("channel", "list").Run() == nil
	}, time.Minute, time.Second)

	for _, channel := range channels {
		f.joinChannel(channel)
	}
	f.deploy(channels)

	snapshots := make(map[string]string, len(channels))
	for _, channel := range channels {
		snapshots[channel] = f.snapshot(channel)
	}
	return snapshots
}

func (f *fabricNetwork) joinChannel(channel string) {
	f.t.Helper()
	admin := []string{
		"-o", "localhost:18053",
		"--ca-file", f.ordererCA(),
		"--client-cert", f.crypto("ordererOrganizations", "example.com", "orderers", "orderer.example.com", "tls", "server.crt"),
		"--client-key", f.crypto("ordererOrganizations", "example.com", "orderers", "orderer.example.com", "tls", "server.key"),
	}
	f.run(f.bin("osnadmin"), append([]string{"channel", "join", "--channelID", channel, "--config-block", filepath.Join(f.root, channel+".block")}, admin...)...)
	f.runPeer("channel", "join", "-b", filepath.Join(f.root, channel+".block"))
	require.Eventually(f.t, func() bool {
		args := append([]string{"channel", "fetch", "newest", filepath.Join(f.root, channel+"-newest.block"), "-c", channel}, f.ordererArgs()...)
		return f.peerCommand(args...).Run() == nil
	}, time.Minute, time.Second)
}

func (f *fabricNetwork) deploy(channels []string) {
	f.t.Helper()
	chaincodes := []struct {
		name        string
		path        string
		collections string
		packageID   string
	}{
		{name: "basic", path: filepath.Join(f.samples, "asset-transfer-basic", "chaincode-go")},
		{name: "private", path: filepath.Join(f.samples, "asset-transfer-private-data", "chaincode-go"), collections: filepath.Join(f.root, "collections.json")},
	}
	for i := range chaincodes {
		packagePath := filepath.Join(f.root, chaincodes[i].name+".tar.gz")
		label := chaincodes[i].name + "_1.0"
		f.runPeer("lifecycle", "chaincode", "package", packagePath, "--path", chaincodes[i].path, "--lang", "golang", "--label", label)
		chaincodes[i].packageID = strings.TrimSpace(f.peerOutput("lifecycle", "chaincode", "calculatepackageid", packagePath))
		f.runPeer("lifecycle", "chaincode", "install", packagePath)
	}
	for _, channel := range channels {
		for _, chaincode := range chaincodes {
			approve := append([]string{"lifecycle", "chaincode", "approveformyorg"}, f.ordererArgs()...)
			approve = append(approve, "--channelID", channel, "--name", chaincode.name, "--version", "1.0", "--package-id", chaincode.packageID, "--sequence", "1")
			commit := append([]string{"lifecycle", "chaincode", "commit"}, f.ordererArgs()...)
			commit = append(commit, "--channelID", channel, "--name", chaincode.name, "--version", "1.0", "--sequence", "1", "--peerAddresses", "localhost:18051", "--tlsRootCertFiles", f.peerCA())
			if chaincode.collections != "" {
				approve = append(approve, "--collections-config", chaincode.collections)
				commit = append(commit, "--collections-config", chaincode.collections)
			}
			f.runPeer(approve...)
			f.runPeer(commit...)
		}
		invoke := append([]string{"chaincode", "invoke"}, f.ordererArgs()...)
		f.runPeer(append(invoke, "-C", channel, "-n", "basic", "--peerAddresses", "localhost:18051", "--tlsRootCertFiles", f.peerCA(), "--waitForEvent", "-c", `{"function":"InitLedger","Args":[]}`)...)
		asset := base64.StdEncoding.EncodeToString([]byte(`{"objectType":"asset","assetID":"asset-private-1","color":"blue","size":5,"appraisedValue":100}`))
		f.runPeer(append(invoke, "-C", channel, "-n", "private", "--peerAddresses", "localhost:18051", "--tlsRootCertFiles", f.peerCA(), "--waitForEvent", "--transient", fmt.Sprintf(`{"asset_properties":"%s"}`, asset), "-c", `{"function":"CreateAsset","Args":[]}`)...)
	}
}

func ensureFabricSamples(t *testing.T, repository string) string {
	t.Helper()
	commitBytes, err := os.ReadFile(filepath.Join(repository, "fabric-samples.commit"))
	require.NoError(t, err)
	commit := strings.TrimSpace(string(commitBytes))
	require.NotEmpty(t, commit)
	destination := filepath.Join(repository, ".cache", "fabric-samples", commit)
	if output, err := exec.Command("git", "-C", destination, "rev-parse", "HEAD").Output(); err == nil {
		require.Equal(t, commit, strings.TrimSpace(string(output)))
		return destination
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(destination), 0o700))
	command := exec.Command("git", "clone", "--filter=blob:none", "--no-checkout", "https://github.com/hyperledger/fabric-samples.git", destination)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	command = exec.Command("git", "-C", destination, "fetch", "--depth=1", "origin", commit)
	output, err = command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	command = exec.Command("git", "-C", destination, "checkout", "--detach", commit)
	output, err = command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return destination
}

func (f *fabricNetwork) snapshot(channel string) string {
	f.t.Helper()
	output := f.peerOutput("channel", "getinfo", "-c", channel)
	start := strings.LastIndex(output, "Blockchain info: ")
	require.NotEqual(f.t, -1, start, output)
	var info struct {
		Height uint64 `json:"height"`
	}
	require.NoError(f.t, json.Unmarshal([]byte(strings.TrimSpace(output[start+len("Blockchain info: "):])), &info))
	require.NotZero(f.t, info.Height)
	block := strconv.FormatUint(info.Height-1, 10)
	remote := "/var/hyperledger/production/snapshots/completed/" + channel + "/" + block
	f.runPeer("snapshot", "submitrequest", "-c", channel, "-b", "0", "--peerAddress", "localhost:18051", "--tlsRootCertFile", f.peerCA())
	require.Eventually(f.t, func() bool {
		return f.composeCommand("exec", "-T", "peer", "test", "-f", remote+"/_snapshot_signable_metadata.json").Run() == nil
	}, time.Minute, time.Second)
	destination := filepath.Join(f.root, "snapshots", channel, "block-"+block)
	require.NoError(f.t, os.MkdirAll(destination, 0o700))
	f.compose("cp", "peer:"+remote+"/.", destination)
	return destination
}

func (f *fabricNetwork) bin(name string) string { return filepath.Join(f.home, "bin", name) }
func (f *fabricNetwork) crypto(parts ...string) string {
	return filepath.Join(append([]string{f.root, "crypto"}, parts...)...)
}
func (f *fabricNetwork) peerCA() string {
	return f.crypto("peerOrganizations", "org1.example.com", "tlsca", "tlsca.org1.example.com-cert.pem")
}
func (f *fabricNetwork) ordererCA() string {
	return f.crypto("ordererOrganizations", "example.com", "tlsca", "tlsca.example.com-cert.pem")
}
func (f *fabricNetwork) ordererArgs() []string {
	return []string{"--orderer", "localhost:18050", "--ordererTLSHostnameOverride", "orderer.example.com", "--tls", "--cafile", f.ordererCA()}
}
func (f *fabricNetwork) runPeer(args ...string) { f.runCommand(f.peerCommand(args...)) }
func (f *fabricNetwork) peerOutput(args ...string) string {
	return f.output(f.peerCommand(args...))
}
func (f *fabricNetwork) peerCommand(args ...string) *exec.Cmd {
	command := f.command(f.bin("peer"), args...)
	command.Env = append(command.Env,
		"FABRIC_CFG_PATH="+filepath.Join(f.home, "config"),
		"CORE_PEER_TLS_ENABLED=true",
		"CORE_PEER_LOCALMSPID=Org1MSP",
		"CORE_PEER_MSPCONFIGPATH="+f.crypto("peerOrganizations", "org1.example.com", "users", "Admin@org1.example.com", "msp"),
		"CORE_PEER_ADDRESS=localhost:18051",
		"CORE_PEER_TLS_ROOTCERT_FILE="+f.peerCA(),
	)
	return command
}
func (f *fabricNetwork) compose(args ...string) { f.runCommand(f.composeCommand(args...)) }
func (f *fabricNetwork) composeCommand(args ...string) *exec.Cmd {
	command := f.command("docker", append([]string{"compose", "-f", filepath.Join(f.root, "compose.yaml")}, args...)...)
	command.Env = append(command.Env, "FABRIC_VERSION="+f.version, "STATE_DATABASE="+f.stateDB)
	return command
}
func (f *fabricNetwork) run(name string, args ...string) { f.runCommand(f.command(name, args...)) }
func (f *fabricNetwork) command(name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	command.Dir = f.root
	command.Env = os.Environ()
	return command
}
func (f *fabricNetwork) runCommand(command *exec.Cmd) {
	f.t.Helper()
	output, err := command.CombinedOutput()
	if err == nil {
		return
	}
	logs, _ := f.composeCommand("logs", "peer", "couchdb").CombinedOutput()
	require.NoError(f.t, err, "%s failed\n%s\n%s", command.String(), output, logs)
}
func (f *fabricNetwork) output(command *exec.Cmd) string {
	f.t.Helper()
	output, err := command.CombinedOutput()
	require.NoError(f.t, err, "%s failed\n%s", command.String(), output)
	return string(output)
}

func ensureFabricRelease(t *testing.T, destination, version string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(destination, "bin", "peer")); err == nil {
		return
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(destination), 0o700))
	temporary := t.TempDir()
	asset := fmt.Sprintf("hyperledger-fabric-%s-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH, version)
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Get("https://github.com/hyperledger/fabric/releases/download/v" + version + "/" + asset)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, extractFabricRelease(temporary, response.Body))
	require.NoError(t, os.RemoveAll(destination))
	require.NoError(t, os.Rename(temporary, destination))
}

func extractFabricRelease(destination string, source io.Reader) error {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	archive := tar.NewReader(gzipReader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		path := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, archive)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", header.Name)
		}
	}
}
