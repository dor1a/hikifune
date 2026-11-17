// Package labels reads the hikifune.* container labels.
package labels

import (
	"fmt"
	"strconv"
)

// Protected turns automatic updates off for a container.
const Protected = "hikifune.protected"

// Policy is what a container's labels ask of hikifune. The zero value is a
// container with no hikifune label, which is checked and updated.
type Policy struct {
	Protected bool
}

// Parse reads the hikifune labels of one container. A label with no value
// (docker run --label hikifune.protected) counts as true. A value that is not
// a boolean also counts as protected, so a typo never leads to an update, and
// the error says which label to fix.
func Parse(labels map[string]string) (Policy, error) {
	var p Policy
	v, ok := labels[Protected]
	if !ok {
		return p, nil
	}
	if v == "" {
		p.Protected = true
		return p, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.Protected = true
		return p, fmt.Errorf("label %s=%q is not true or false, treating it as true", Protected, v)
	}
	p.Protected = b
	return p, nil
}
