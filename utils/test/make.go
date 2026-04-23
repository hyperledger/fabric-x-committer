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
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

// Run executes the specified command and returns the corresponding process.
// It is important to note that the underlying invocation function (Invoke)
// returns only when either process.Ready or process.Wait has been read.
// Consequently, the caller only needs to read process.Wait to wait for the
// process to complete and capture any errors that may have occurred during execution.
//
//nolint:ireturn
func Run(cmd *exec.Cmd, name, startCheck string) ifrit.Process {
	return ifrit.Invoke(ginkgomon.New(ginkgomon.Config{
		Command:           cmd,
		Name:              name,
		AnsiColorCode:     "",
		StartCheck:        startCheck,
		StartCheckTimeout: 0,
		Cleanup: func() {
		},
	}))
}

// Make runs a make command with the specified rules.
func Make(rules ...string) error {
	dir, err := os.Getwd()
	if err != nil {
		return errors.Wrap(err, "failed to get current working directory")
	}
	buildCmd := exec.Command("make", rules...)
	buildCmd.Dir = path.Clean(path.Join(dir, "../.."))
	makeRun := Run(buildCmd, "make", "")
	log.Println("wait")
	select {
	case err = <-makeRun.Wait():
		return errors.Wrap(err, "failed to run make command")
	case <-time.After(3 * time.Minute):
		makeRun.Signal(os.Kill)
		return errors.New("make command timed out")
	}
}
