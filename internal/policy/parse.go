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

package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseQuotaSize parses a size string like "10Gi", "100Mi" into bytes
func ParseQuotaSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	var multiplier int64 = 1
	var numStr string

	// Check for unit suffix
	s = strings.ToUpper(s)
	switch {
	case strings.HasSuffix(s, "TI"):
		multiplier = 1024 * 1024 * 1024 * 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(s, "GI"):
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(s, "MI"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(s, "KI"):
		multiplier = 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(s, "T"):
		multiplier = 1000 * 1000 * 1000 * 1000
		numStr = s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		multiplier = 1000 * 1000 * 1000
		numStr = s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		multiplier = 1000 * 1000
		numStr = s[:len(s)-1]
	case strings.HasSuffix(s, "K"):
		multiplier = 1000
		numStr = s[:len(s)-1]
	default:
		numStr = s
	}

	value, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %s", numStr)
	}

	return int64(value * float64(multiplier)), nil
}
