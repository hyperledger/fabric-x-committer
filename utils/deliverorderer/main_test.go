/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"testing"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
)

func TestMain(m *testing.M) {
	_ = factory.InitFactories(nil)
	m.Run()
}
