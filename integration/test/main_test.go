/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"log"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestMain(m *testing.M) {
	err := test.Make("build")
	if err != nil {
		log.Fatal(err)
	}
	// Waiting a seconds solves a bug where the binaries are not yet accessible by the filesystem.
	time.Sleep(2 * time.Second)
	testdb.RunTestMain(m)
}
