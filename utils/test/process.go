/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/cockroachdb/errors"
)

// Process represents a managed external process with lifecycle control.
// This is a simple, test-friendly wrapper around os/exec.Cmd.
type Process struct {
	cmd    *exec.Cmd
	name   string
	waitCh chan error
}

// NewProcess creates and starts a new managed process.
// The process starts immediately and streams output to stdout with a name prefix.
func NewProcess(cmd *exec.Cmd, name string) (*Process, error) {
	p := &Process{
		cmd:    cmd,
		name:   name,
		waitCh: make(chan error, 1),
	}

	// Setup stdout/stderr pipes for output streaming.
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create stdout pipe")
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create stderr pipe")
	}

	// Start the process.
	if err = p.cmd.Start(); err != nil {
		return nil, errors.Wrapf(err, "failed to start process %s", p.name)
	}

	go func() {
		// Stream output in background goroutines.
		var wg sync.WaitGroup
		wg.Go(func() {
			p.streamOutput(stdout)
		})
		wg.Go(func() {
			p.streamOutput(stderr)
		})

		// Wait for process to complete.
		err = p.cmd.Wait()
		wg.Wait()
		p.waitCh <- err
	}()

	return p, nil
}

func (p *Process) streamOutput(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Printf("\033[0;31m[%-15s]\033[0m %s\n", p.name, line)
	}
}

// Signal sends a signal to the process.
func (p *Process) Signal(sig os.Signal) {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// Wait returns a channel that will receive an error when the process exits.
// If the process exits successfully, nil is sent. Otherwise, the error is sent.
func (p *Process) Wait() <-chan error {
	return p.waitCh
}
