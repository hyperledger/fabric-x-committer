/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"log"
	"os"
	"os/exec"
	"path"
	"time"

	"github.com/cockroachdb/errors"
)

// Make runs a make command with the specified rules.
func Make(rules ...string) error {
	dir, err := os.Getwd()
	if err != nil {
		return errors.Wrap(err, "failed to get current working directory")
	}
	buildCmd := exec.Command("make", rules...)
	buildCmd.Dir = path.Clean(path.Join(dir, "../.."))
	makeRun, err := NewProcess(buildCmd, "make")
	if err != nil {
		return err
	}
	log.Println("wait")
	select {
	case err = <-makeRun.Wait():
		return errors.Wrap(err, "failed to run make command")
	case <-time.After(3 * time.Minute):
		makeRun.Signal(os.Kill)
		return errors.New("make command timed out")
	}
}
