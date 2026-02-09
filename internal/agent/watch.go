/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"log/slog"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// watchPVs watches for PV changes
func (a *QuotaAgent) watchPVs(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		watcher, err := a.client.CoreV1().PersistentVolumes().Watch(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Error("Failed to start PV watch", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for event := range watcher.ResultChan() {
			pv, ok := event.Object.(*v1.PersistentVolume)
			if !ok {
				continue
			}

			switch event.Type {
			case watch.Added, watch.Modified:
				if a.shouldProcessPV(pv) {
					if err := a.ensureQuota(ctx, pv); err != nil {
						slog.Error("Failed to ensure quota", "pv", pv.Name, "error", err)
					}
				}
			case watch.Deleted:
				a.mu.Lock()
				nfsPath := a.getNFSPath(pv)
				if nfsPath != "" {
					localPath := a.nfsPathToLocal(nfsPath)
					delete(a.appliedQuotas, localPath)
				}
				a.mu.Unlock()
				slog.Debug("PV deleted, quota tracking removed", "pv", pv.Name)
			}
		}

		slog.Warn("PV watch ended, restarting...")
		time.Sleep(1 * time.Second)
	}
}
