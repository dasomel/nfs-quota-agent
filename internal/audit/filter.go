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

package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dasomel/nfs-quota-agent/internal/util"
)

// Filter filters audit entries
type Filter struct {
	Action    Action
	PVName    string
	Namespace string
	Path      string
	StartTime time.Time
	EndTime   time.Time
	OnlyFails bool
}

// Matches checks if an entry matches the filter
func (f Filter) Matches(entry Entry) bool {
	if f.Action != "" && entry.Action != f.Action {
		return false
	}
	if f.PVName != "" && entry.PVName != f.PVName {
		return false
	}
	if f.Namespace != "" && entry.Namespace != f.Namespace {
		return false
	}
	if f.Path != "" && entry.Path != f.Path {
		return false
	}
	if !f.StartTime.IsZero() && entry.Timestamp.Before(f.StartTime) {
		return false
	}
	if !f.EndTime.IsZero() && entry.Timestamp.After(f.EndTime) {
		return false
	}
	if f.OnlyFails && entry.Success {
		return false
	}
	return true
}

// QueryLog queries the audit log file
func QueryLog(filePath string, filter Filter) ([]Entry, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []Entry
	decoder := json.NewDecoder(file)

	for {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil {
			if err == io.EOF {
				break
			}
			continue // Skip malformed entries
		}

		if filter.Matches(entry) {
			entries = append(entries, entry)
		}
	}

	return entries, nil
}

// PrintEntries prints audit entries in a formatted table
func PrintEntries(entries []Entry, format string) {
	switch format {
	case "json":
		for _, entry := range entries {
			data, _ := json.Marshal(entry)
			fmt.Println(string(data))
		}
	case "table":
		fmt.Printf("%-20s %-8s %-30s %-40s %-10s %s\n",
			"TIMESTAMP", "ACTION", "PV_NAME", "PATH", "STATUS", "QUOTA")
		fmt.Println(strings.Repeat("-", 120))

		for _, entry := range entries {
			status := "OK"
			if !entry.Success {
				status = "FAIL"
			}
			quota := ""
			if entry.NewQuota > 0 {
				quota = util.FormatBytes(entry.NewQuota)
			}

			pvName := entry.PVName
			if len(pvName) > 30 {
				pvName = pvName[:27] + "..."
			}
			path := entry.Path
			if len(path) > 40 {
				path = "..." + path[len(path)-37:]
			}

			fmt.Printf("%-20s %-8s %-30s %-40s %-10s %s\n",
				entry.Timestamp.Format("2006-01-02 15:04:05"),
				entry.Action,
				pvName,
				path,
				status,
				quota,
			)
		}
	default:
		for _, entry := range entries {
			fmt.Printf("[%s] %s: %s -> %s (success=%v)\n",
				entry.Timestamp.Format(time.RFC3339),
				entry.Action,
				entry.PVName,
				entry.Path,
				entry.Success,
			)
		}
	}
}
