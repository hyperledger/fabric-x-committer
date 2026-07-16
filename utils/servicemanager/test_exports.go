/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicemanager

import "google.golang.org/grpc"

// GetAllActiveConnections returns all the manager's active connections.
func GetAllActiveConnections(m *Manager) []*grpc.ClientConn {
	workers := m.workers.Load()
	if workers == nil {
		return nil
	}
	res := make([]*grpc.ClientConn, len(*workers))
	for i, w := range *workers {
		res[i] = w.conn
	}
	return res
}

// GetAllPendingTaskCount returns the count of pending tasks being processed by each worker.
func GetAllPendingTaskCount(m *Manager) []int {
	workers := m.workers.Load()
	if workers == nil {
		return nil
	}
	res := make([]int, len(*workers))
	for i, w := range *workers {
		res[i] = w.tasksBeingProcessed.Count()
	}
	return res
}
