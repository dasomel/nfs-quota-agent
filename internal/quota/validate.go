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

package quota

import (
	"fmt"
	"unicode"
)

// validateQuotaArg rejects empty strings, strings containing whitespace,
// single or double quotes, or control characters.
func validateQuotaArg(kind, value string) error {
	if value == "" {
		return fmt.Errorf("invalid %s %q: cannot be empty", kind, value)
	}

	for _, r := range value {
		if unicode.IsSpace(r) {
			return fmt.Errorf("invalid %s %q: contains whitespace", kind, value)
		}
		if r == '\'' || r == '"' {
			return fmt.Errorf("invalid %s %q: contains quotes", kind, value)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid %s %q: contains control characters", kind, value)
		}
	}

	return nil
}

// validateProjectName additionally rejects the ':' that separates the fields
// of an /etc/projid line.
//
// Project names come straight from the nfs.io/project-name annotation, so
// anyone able to create or patch a PV controls this string. A name such as
// "evil:999" writes the line "evil:999:7", which parses back as the name
// "evil" under the ID "999:7". That ID fails ParseUint, so the line is skipped
// when the taken-ID set is built and the real ID 7 reads as free — handing it
// to a second project and bleeding quota between two unrelated volumes.
//
// Names are rejected rather than sanitised: rewriting one silently changes
// which quota an operator's PV is bound to, whereas a rejection surfaces on
// the PV as a failed status. Paths are exempt because /etc/projects lines
// parse as "id:path" with SplitN(line, ":", 2), so a colon inside the path is
// harmless. Newlines, the other delimiter that would corrupt these files, are
// already covered by the control-character check above.
func validateProjectName(projectName string) error {
	if err := validateQuotaArg("projectName", projectName); err != nil {
		return err
	}
	for _, r := range projectName {
		if r == ':' {
			return fmt.Errorf("invalid projectName %q: contains ':', which separates the fields of an /etc/projid entry", projectName)
		}
	}
	return nil
}
