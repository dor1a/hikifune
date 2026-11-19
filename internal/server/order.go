package server

import (
	"sort"

	"github.com/dor1a/hikifune/internal/protocol"
)

// order puts the containers a compose service depends on before it, within
// each compose project (R29). Otherwise containers go by name. A cycle,
// which compose itself refuses, falls back to name order for the rest.
func order(cs []protocol.Checked) []protocol.Checked {
	sorted := append([]protocol.Checked(nil), cs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	key := func(c protocol.Checked) string { return c.ComposeProject + "\x00" + c.ComposeService }
	// replicas counts the containers of each service, all of which go first.
	replicas := map[string]int{}
	for _, c := range sorted {
		if c.ComposeProject != "" && c.ComposeService != "" {
			replicas[key(c)]++
		}
	}
	// waiting[i] counts the containers sorted[i] depends on that are still to go.
	waiting := make([]int, len(sorted))
	for i, c := range sorted {
		for _, d := range c.DependsOn {
			if c.ComposeProject != "" && d != c.ComposeService {
				waiting[i] += replicas[c.ComposeProject+"\x00"+d]
			}
		}
	}
	out := make([]protocol.Checked, 0, len(sorted))
	used := make([]bool, len(sorted))
	for len(out) < len(sorted) {
		next := -1
		for i := range sorted {
			if !used[i] && waiting[i] == 0 {
				next = i
				break
			}
		}
		if next < 0 { // a cycle
			for i := range sorted {
				if !used[i] {
					next = i
					break
				}
			}
		}
		used[next] = true
		done := sorted[next]
		out = append(out, done)
		for i, c := range sorted {
			if used[i] || c.ComposeProject != done.ComposeProject || done.ComposeService == "" {
				continue
			}
			for _, d := range c.DependsOn {
				if d == done.ComposeService && waiting[i] > 0 {
					waiting[i]--
				}
			}
		}
	}
	return out
}
