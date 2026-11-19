// Package update recreates one container on the image its reference names
// now, and rolls it back when the new container does not come up.
//
// The new container is built from the old one's inspect output as JSON,
// never converted to another form (R9). What came from the old image is
// dropped so the new image's defaults apply; everything set on the container
// is kept (R22).
package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// composeImageLabel holds the ID of the image compose created a container
// from. Compose recreates a container whose label differs from the image it
// would run.
const composeImageLabel = "com.docker.compose.image"

// imageDefaults are the Config fields a container takes from its image as a
// whole when it does not set them.
var imageDefaults = []string{"Healthcheck", "WorkingDir", "User", "StopSignal", "Shell", "OnBuild", "ArgsEscaped"}

// assigned are the endpoint fields Docker fills in, as opposed to the ones
// that were configured.
var assigned = []string{
	"NetworkID", "EndpointID", "Gateway", "IPAddress", "IPPrefixLen", "IPv6Gateway",
	"GlobalIPv6Address", "GlobalIPv6PrefixLen", "MacAddress", "DNSNames",
}

// spec is what recreating one container sends to the daemon.
type spec struct {
	config      map[string]any // the Config fields of the create body
	hostConfig  map[string]any
	primary     string         // the network in the create body, "" for none
	primaryKey  string         // what the create body names it by: HostConfig.NetworkMode
	endpoints   map[string]any // network name -> endpoint settings, the primary one too
	connectLast []string       // the other networks, connected before start, sorted
}

// body is the JSON the create endpoint takes.
func (s spec) body() (json.RawMessage, error) {
	b := make(map[string]any, len(s.config)+2)
	for k, v := range s.config {
		b[k] = v
	}
	b["HostConfig"] = s.hostConfig
	if s.primary != "" {
		b["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{s.primaryKey: s.endpoints[s.primary]},
		}
	}
	return json.Marshal(b)
}

// endpoint is the JSON of one endpoint's settings.
func (s spec) endpoint(network string) (json.RawMessage, error) {
	return json.Marshal(s.endpoints[network])
}

// decode reads JSON keeping numbers as they were written, so they go back to
// the daemon unchanged.
func decode(raw []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("empty document")
	}
	return m, nil
}

// build makes the spec of the new container from the old container's and its
// image's inspect documents. oldImageID and newImageID are the image IDs the
// old container runs and the pull brought.
func build(container, oldImage []byte, oldImageID, newImageID string) (spec, error) {
	ct, err := decode(container)
	if err != nil {
		return spec{}, fmt.Errorf("read the container: %w", err)
	}
	img, err := decode(oldImage)
	if err != nil {
		return spec{}, fmt.Errorf("read the old image: %w", err)
	}
	id, _ := ct["Id"].(string)
	if id == "" {
		return spec{}, errors.New("the container has no Id")
	}
	cfg := object(ct["Config"])
	hc := object(ct["HostConfig"])
	if cfg == nil || hc == nil {
		return spec{}, errors.New("the container has no Config or HostConfig")
	}
	imgCfg := object(img["Config"])
	if imgCfg == nil {
		imgCfg = map[string]any{}
	}

	setList(cfg, "Env", without(cfg["Env"], imgCfg["Env"]))
	labels := withoutPairs(cfg["Labels"], imgCfg["Labels"])
	if labels != nil && labels[composeImageLabel] == oldImageID {
		labels[composeImageLabel] = newImageID
	}
	setObject(cfg, "Labels", labels)
	setObject(cfg, "ExposedPorts", withoutKeys(cfg["ExposedPorts"], imgCfg["ExposedPorts"]))
	setObject(cfg, "Volumes", withoutKeys(cfg["Volumes"], imgCfg["Volumes"]))
	for _, k := range imageDefaults {
		if same(cfg[k], imgCfg[k]) {
			delete(cfg, k)
		}
	}
	// Docker applies the image's Cmd only to a container without its own
	// entrypoint, so Cmd can go only together with Entrypoint. An empty list
	// is not a missing one here: it clears what the image sets.
	if reflect.DeepEqual(cfg["Entrypoint"], imgCfg["Entrypoint"]) {
		delete(cfg, "Entrypoint")
		if reflect.DeepEqual(cfg["Cmd"], imgCfg["Cmd"]) {
			delete(cfg, "Cmd")
		}
	}
	mode, _ := hc["NetworkMode"].(string)
	if host, _ := cfg["Hostname"].(string); host == "" || strings.HasPrefix(id, host) && len(host) == 12 ||
		mode == "host" || strings.HasPrefix(mode, "container:") {
		delete(cfg, "Hostname")
	}
	// The container-wide MAC address is deprecated since API 1.44 and refused
	// from 1.52. A MAC address Docker assigned is not kept either.
	delete(cfg, "MacAddress")

	if err := keepAnonymousVolumes(hc, ct["Mounts"]); err != nil {
		return spec{}, err
	}

	s := spec{config: cfg, hostConfig: hc, endpoints: map[string]any{}}
	if err := s.networks(ct, id, mode); err != nil {
		return spec{}, err
	}
	return s, nil
}

// networks fills in the endpoints: what was configured, without what Docker
// assigned and without the old container's ID among the aliases.
func (s *spec) networks(ct map[string]any, id, mode string) error {
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") {
		return nil
	}
	nets := object(object(ct["NetworkSettings"])["Networks"])
	if len(nets) == 0 {
		return nil
	}
	// The network mode names the primary network by name, or by ID when the
	// container was created with one.
	primary := mode
	if primary == "" || primary == "default" {
		primary = "bridge"
	}
	if _, ok := nets[primary]; !ok {
		found := ""
		for name, raw := range nets {
			if nid, _ := object(raw)["NetworkID"].(string); nid != "" && strings.HasPrefix(nid, primary) {
				found = name
			}
		}
		if found == "" && len(nets) == 1 {
			for name := range nets {
				found = name
			}
		}
		if found == "" {
			return fmt.Errorf("network mode %q names none of the container's networks", mode)
		}
		primary = found
	}
	for name, raw := range nets {
		ep := object(raw)
		if ep == nil {
			ep = map[string]any{}
		}
		for _, k := range assigned {
			delete(ep, k)
		}
		var aliases []any
		for _, a := range list(ep["Aliases"]) {
			if s, _ := a.(string); s != "" && s != id && !(len(s) == 12 && strings.HasPrefix(id, s)) {
				aliases = append(aliases, a)
			}
		}
		setList(ep, "Aliases", aliases)
		s.endpoints[name] = ep
	}
	s.primaryKey = primary
	if mode != "" && mode != "default" {
		s.primaryKey = mode
	}
	s.primary = primary
	for name := range nets {
		if name != primary {
			s.connectLast = append(s.connectLast, name)
		}
	}
	slices.Sort(s.connectLast)
	return nil
}

// keepAnonymousVolumes mounts the volumes the old container got without a
// name, from the image's VOLUME lines or a compose volume with no source,
// again at the same paths. Left out, the new container would get new empty
// volumes there.
func keepAnonymousVolumes(hc map[string]any, rawMounts any) error {
	volumes := map[string]map[string]any{} // destination -> mount point
	for _, raw := range list(rawMounts) {
		m := object(raw)
		if m["Type"] == "volume" {
			if dst, _ := m["Destination"].(string); dst != "" {
				volumes[dst] = m
			}
		}
	}
	covered := map[string]bool{}
	binds := list(hc["Binds"])
	for i, raw := range binds {
		b, _ := raw.(string)
		parts := strings.Split(b, ":")
		switch {
		case len(parts) >= 2 && strings.HasPrefix(parts[1], "/"):
			covered[parts[1]] = true
		case strings.HasPrefix(parts[0], "/"):
			// A path with no source, maybe with a mode, is an anonymous
			// volume: name the one the old container got.
			v, ok := volumes[parts[0]]
			name, _ := v["Name"].(string)
			if !ok || name == "" {
				return fmt.Errorf("the anonymous volume at %s is not among the container's mounts", parts[0])
			}
			binds[i] = name + ":" + b
			covered[parts[0]] = true
		}
	}
	mounts := list(hc["Mounts"])
	for _, raw := range mounts {
		m := object(raw)
		target, _ := m["Target"].(string)
		covered[target] = true
		if m["Type"] == "volume" && isEmpty(m["Source"]) {
			v, ok := volumes[target]
			if !ok {
				return fmt.Errorf("the anonymous volume at %s is not among the container's mounts", target)
			}
			m["Source"] = v["Name"]
		}
	}
	var dsts []string
	for dst := range volumes {
		if !covered[dst] {
			dsts = append(dsts, dst)
		}
	}
	slices.Sort(dsts)
	for _, dst := range dsts {
		v := volumes[dst]
		name, _ := v["Name"].(string)
		if name == "" {
			return fmt.Errorf("the volume at %s has no name", dst)
		}
		m := map[string]any{"Type": "volume", "Source": name, "Target": dst}
		if rw, ok := v["RW"].(bool); ok && !rw {
			m["ReadOnly"] = true
		}
		mounts = append(mounts, m)
	}
	if len(mounts) > 0 {
		hc["Mounts"] = mounts
	}
	return nil
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// setList sets m[key] to v, or deletes the key when v is empty.
func setList(m map[string]any, key string, v []any) {
	if len(v) == 0 {
		delete(m, key)
		return
	}
	m[key] = v
}

// setObject sets m[key] to v, or deletes the key when v is empty.
func setObject(m map[string]any, key string, v map[string]any) {
	if len(v) == 0 {
		delete(m, key)
		return
	}
	m[key] = v
}

// without returns the entries of a that b does not have.
func without(a, b any) []any {
	var out []any
	for _, v := range list(a) {
		if !slices.ContainsFunc(list(b), func(w any) bool { return same(v, w) }) {
			out = append(out, v)
		}
	}
	return out
}

// withoutPairs returns the keys of a whose value b does not have for them.
func withoutPairs(a, b any) map[string]any {
	am, bm := object(a), object(b)
	var out map[string]any
	for k, v := range am {
		if w, ok := bm[k]; ok && same(v, w) {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	return out
}

// withoutKeys returns the keys of a that b does not have.
func withoutKeys(a, b any) map[string]any {
	am, bm := object(a), object(b)
	var out map[string]any
	for k, v := range am {
		if _, ok := bm[k]; ok {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	return out
}

// isEmpty reports whether v is what an absent field reads as.
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case json.Number:
		return x == "0"
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// same compares two JSON values, taking a missing field, null and an empty
// value as the same.
func same(a, b any) bool {
	if isEmpty(a) && isEmpty(b) {
		return true
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range x {
			if !same(v, y[k]) {
				return false
			}
		}
		for k, v := range y {
			if _, ok := x[k]; !ok && !isEmpty(v) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !same(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x == y
	}
	return a == b
}
