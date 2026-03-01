/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"io"
	"log"
	"os"
	"testing"

	"google.golang.org/grpc/grpclog"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestMain(m *testing.M) {
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, os.Stderr))
	err := test.Make("build-test-node-image", "build-release-image")
	if err != nil {
		//nolint:revive // false positive.
		log.Fatal(err)
	}
	m.Run()
}
