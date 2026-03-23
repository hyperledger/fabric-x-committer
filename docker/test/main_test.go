/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"log"
	"testing"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestMain(m *testing.M) {
	err := test.Make("build-image-test-node", "build-image-release")
	if err != nil {
		log.Fatal(err) //nolint:revive,nolintlint // false positive.
	}
	m.Run()
}
