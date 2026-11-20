package update

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// compare checks the new container against what was sent for it (R23):
// every value sent is there, it runs newImageID, every network is attached,
// and every mount of the old container is there with the same source. What
// the new image adds is expected and not compared.
func compare(s spec, oldRaw, newRaw []byte, newImageID string) error {
	old, err := decode(oldRaw)
	if err != nil {
		return fmt.Errorf("read the old container: %w", err)
	}
	ct, err := decode(newRaw)
	if err != nil {
		return fmt.Errorf("read the new container: %w", err)
	}
	if got, _ := ct["Image"].(string); got != newImageID {
		return fmt.Errorf("it runs image %s, not the pulled %s", got, newImageID)
	}
	if err := contains("Config", s.config, object(ct["Config"])); err != nil {
		return err
	}
	if err := contains("HostConfig", s.hostConfig, object(ct["HostConfig"])); err != nil {
		return err
	}
	nets := object(object(ct["NetworkSettings"])["Networks"])
	for name, want := range s.endpoints {
		got, ok := nets[name]
		if !ok {
			return fmt.Errorf("network %s is not attached", name)
		}
		if err := contains("network "+name, object(want), object(got)); err != nil {
			return err
		}
	}
	return sameMounts(list(old["Mounts"]), list(ct["Mounts"]))
}

// contains reports the first value in want that got does not have. Lists of
// strings that are sets to Docker (environment variables, aliases) may come
// back in another order or with more entries.
func contains(path string, want, got map[string]any) error {
	for k, w := range want {
		p := path + "." + k
		g, present := got[k]
		switch {
		case !present && w != nil:
			// A key of a label, port or volume map carries no value, so
			// only its presence shows it was applied.
			return fmt.Errorf("%s is missing", p)
		case p == "Config.Env":
			// Values may be secrets, so only names are reported.
			for _, v := range list(w) {
				if !slices.ContainsFunc(list(g), func(x any) bool { return same(v, x) }) {
					name, _, _ := strings.Cut(fmt.Sprint(v), "=")
					return fmt.Errorf("%s: %s is missing or has another value", p, name)
				}
			}
		case setLike(p):
			for _, v := range list(w) {
				if !slices.ContainsFunc(list(g), func(x any) bool { return same(v, x) }) {
					return fmt.Errorf("%s: %s is missing", p, show(v))
				}
			}
		case object(w) != nil && object(g) != nil:
			if err := contains(p, object(w), object(g)); err != nil {
				return err
			}
		case list(w) != nil && list(g) != nil:
			lw, lg := list(w), list(g)
			if len(lw) != len(lg) {
				return fmt.Errorf("%s: sent %s, got %s", p, show(w), show(g))
			}
			for i := range lw {
				if object(lw[i]) != nil && object(lg[i]) != nil {
					if err := contains(fmt.Sprintf("%s[%d]", p, i), object(lw[i]), object(lg[i])); err != nil {
						return err
					}
				} else if !same(lw[i], lg[i]) {
					return fmt.Errorf("%s[%d]: sent %s, got %s", p, i, show(lw[i]), show(lg[i]))
				}
			}
		case normalized(p, w, g):
		case !same(w, g):
			return fmt.Errorf("%s: sent %s, got %s", p, show(w), show(g))
		}
	}
	return nil
}

// setLike names the lists whose order and extra entries do not matter,
// besides Config.Env.
func setLike(path string) bool {
	return strings.HasPrefix(path, "network ") && strings.HasSuffix(path, ".Aliases")
}

// normalized covers values the daemon rewrites on create.
func normalized(path string, want, got any) bool {
	switch path {
	case "HostConfig.PidsLimit":
		// Zero and negative limits mean no limit and are stored as none.
		n, ok := want.(json.Number)
		if !ok {
			return false
		}
		v, err := n.Int64()
		return err == nil && v <= 0 && isEmpty(got)
	}
	return false
}

// sameMounts checks that every mount of the old container is in the new one
// at the same path with the same source, so data stays where it was.
func sameMounts(old, cur []any) error {
	byDst := map[string]map[string]any{}
	for _, raw := range cur {
		m := object(raw)
		if dst, _ := m["Destination"].(string); dst != "" {
			byDst[dst] = m
		}
	}
	for _, raw := range old {
		m := object(raw)
		dst, _ := m["Destination"].(string)
		got, ok := byDst[dst]
		if !ok {
			return fmt.Errorf("mount %s is missing", dst)
		}
		if !same(m["Type"], got["Type"]) {
			return fmt.Errorf("mount %s: was %v, now %v", dst, m["Type"], got["Type"])
		}
		key := "Source"
		if m["Type"] == "volume" {
			key = "Name"
		}
		if !same(m[key], got[key]) {
			return fmt.Errorf("mount %s: was %s, now %s", dst, show(m[key]), show(got[key]))
		}
		if !same(m["RW"], got["RW"]) {
			return fmt.Errorf("mount %s: read-write was %v, now %v", dst, m["RW"], got["RW"])
		}
		// A bind's propagation decides whether mounts made inside reach the
		// host, and the other way round.
		if !same(m["Propagation"], got["Propagation"]) {
			return fmt.Errorf("mount %s: propagation was %s, now %s", dst, show(m["Propagation"]), show(got["Propagation"]))
		}
	}
	return nil
}

func show(v any) string {
	if v == nil {
		return "nothing"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	const max = 120
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
