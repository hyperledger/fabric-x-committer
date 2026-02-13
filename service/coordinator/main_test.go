/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"testing"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

func TestMain(m *testing.M) {
	dbtest.RunTestMain(m)
}
