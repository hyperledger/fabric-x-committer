/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"testing"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"

	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestMain(m *testing.M) {
	_ = factory.InitFactories(nil)
	testdb.RunTestMain(m)
}
