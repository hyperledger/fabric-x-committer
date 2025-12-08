/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
)

func TestRequireProtoEqual(t *testing.T) {
	t.Parallel()
	a := &committerpb.View{Id: "a"}
	RequireProtoEqual(t, a, a)
	b := proto.Clone(a)
	RequireProtoEqual(t, a, b)
	c := &committerpb.View{Id: "a"}
	RequireProtoEqual(t, a, c)
}

func TestProtoDiff(t *testing.T) {
	t.Parallel()
	a := []*committerpb.View{
		{Id: "a"}, {Id: "b"},
	}
	msg, haveDiff := protoElemDiff(a, a)
	require.False(t, haveDiff, msg)

	b := []*committerpb.View{
		{Id: "b"}, {Id: "a"},
	}
	msg, haveDiff = protoElemDiff(a, b)
	require.False(t, haveDiff, msg)

	c := []*committerpb.View{{Id: "b"}, {Id: "c"}}
	msg, haveDiff = protoElemDiff(a, c)
	require.True(t, haveDiff, msg)
	t.Log(msg)
}
