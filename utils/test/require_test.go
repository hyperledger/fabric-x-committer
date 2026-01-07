/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"google.golang.org/protobuf/types/known/durationpb"
)

func TestRequireProtoEqual(t *testing.T) {
	t.Parallel()
	a := durationpb.New(5 * time.Minute)
	RequireProtoEqual(t, a, a)
	b := proto.Clone(a)
	RequireProtoEqual(t, a, b)
	c := durationpb.New(5 * time.Minute)
	RequireProtoEqual(t, a, c)
}

func TestProtoDiff(t *testing.T) {
	t.Parallel()
	a := []*durationpb.Duration{
		durationpb.New(5 * time.Minute), durationpb.New(10 * time.Minute),
	}
	msg, haveDiff := protoElemDiff(a, a)
	require.False(t, haveDiff, msg)

	b := []*durationpb.Duration{
		durationpb.New(5 * time.Minute), durationpb.New(10 * time.Minute),
	}
	msg, haveDiff = protoElemDiff(a, b)
	require.False(t, haveDiff, msg)

	c := []*durationpb.Duration{
		durationpb.New(10 * time.Minute), durationpb.New(20 * time.Minute),
	}
	msg, haveDiff = protoElemDiff(a, c)
	require.True(t, haveDiff, msg)
	t.Log(msg)
}
