/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"google.golang.org/grpc/grpclog"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestMain(m *testing.M) {
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, os.Stderr))

	dir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	buildCmd := exec.Command("make", "build")
	buildCmd.Dir = path.Clean(path.Join(dir, "../.."))
	makeRun := runner.Run(buildCmd, "make", "")
	log.Println("wait")
	select {
	case err = <-makeRun.Wait():
		if err != nil {
			log.Fatal(err)
		}
	case <-time.After(3 * time.Minute):
		makeRun.Signal(os.Kill)
		log.Fatalf("Failed to build binaries")
	}

	// Waiting a seconds solves a bug where the binaries are not yet accessible by the filesystem.
	time.Sleep(time.Second)

	testdb.RunTestMain(m)
}
