/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	"golang.org/x/exp/constraints"
)

// ErrActiveStream represents the error when attempting to create a new stream while one is already active.
// The system only allows a single active stream at any given time.
var ErrActiveStream = errors.New("a stream is already active. Only one active stream is allowed at a time")

// FileExists returns true if a file path exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// LazyJSON will lazily marshal a struct for logging purposes.
type LazyJSON struct {
	O      any
	Indent string
}

// String marshals the give object as JSON.
func (lj *LazyJSON) String() string {
	var p []byte
	var err error
	if lj.Indent != "" {
		p, err = json.MarshalIndent(lj.O, "", lj.Indent)
	} else {
		p, err = json.Marshal(lj.O)
	}
	if err != nil {
		return fmt.Sprintf("cannot marshal object: %v", err)
	}
	return string(p)
}

// Must panics given an error.
func Must(err error, msg ...string) {
	if err != nil {
		panic(errors.Wrap(err, strings.Join(msg, " ")))
	}
}

// Range returns a slice containing integers in the range from start (including) to end (excluding).
func Range[T constraints.Integer](start, end T) []T {
	if start >= end {
		return nil
	}
	results := make([]T, 0, end-start)
	for i := start; i < end; i++ {
		results = append(results, i)
	}
	return results
}

// ProcessErr wraps a non-nil error with a message using %w for unwrapping.
// Returns nil if the error is nil, otherwise returns the wrapped error.
// Example to the call of the function: utils.ProcessErr(g.Wait(), "sidecar has been stopped").
func ProcessErr(err error, msg string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", msg, err) //nolint:wrapcheck
	}
	return nil
}

// CountAppearances returns the number of appearances each item have.
func CountAppearances[T comparable](items []T) map[T]int {
	countMap := make(map[T]int)
	for _, item := range items {
		countMap[item]++
	}
	return countMap
}
