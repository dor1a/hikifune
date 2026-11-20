package update

import (
	"encoding/json"
	"strings"
	"testing"
)

// newFrom makes the inspect document Docker would report for a container
// created from s: what was sent, plus the new image's defaults, plus what
// the daemon adds.
func newFrom(t *testing.T, s spec, oldDoc string, edit func(ct map[string]any)) []byte {
	t.Helper()
	old, err := decode([]byte(oldDoc))
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	for k, v := range s.config {
		cfg[k] = v
	}
	cfg["Env"] = append([]any{"PATH=/usr/local/bin:/usr/bin", "IMG=v2"}, list(s.config["Env"])...)
	cfg["Labels"] = map[string]any{"from.image": "2"}
	for k, v := range object(s.config["Labels"]) {
		cfg["Labels"].(map[string]any)[k] = v
	}
	nets := map[string]any{}
	for name, ep := range s.endpoints {
		e := map[string]any{"NetworkID": "n-" + name}
		for k, v := range object(ep) {
			e[k] = v
		}
		e["Aliases"] = append(list(e["Aliases"]), "fedcba987654")
		nets[name] = e
	}
	hc := map[string]any{"PidsLimit": nil, "ShmSize": json.Number("67108864")}
	for k, v := range s.hostConfig {
		hc[k] = v
	}
	ct := map[string]any{
		"Id":              "fedcba987654",
		"Image":           newImage,
		"Config":          cfg,
		"HostConfig":      hc,
		"NetworkSettings": map[string]any{"Networks": nets},
		"Mounts":          old["Mounts"],
	}
	if edit != nil {
		edit(ct)
	}
	b, err := json.Marshal(ct)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCompareAcceptsWhatDockerAdds(t *testing.T) {
	s := mustBuild(t, oldContainer, oldImageDoc)
	if err := compare(s, []byte(oldContainer), newFrom(t, s, oldContainer, nil), newImage); err != nil {
		t.Fatal(err)
	}
}

func TestCompareFindsDifferences(t *testing.T) {
	s := mustBuild(t, oldContainer, oldImageDoc)
	tests := []struct {
		name string
		edit func(ct map[string]any)
		want string
	}{
		{"another image", func(ct map[string]any) { ct["Image"] = "sha256:other" }, "runs image"},
		{"an environment variable lost", func(ct map[string]any) {
			ct["Config"].(map[string]any)["Env"] = []any{"KEEP=mine"}
		}, "Config.Env"},
		{"a label changed", func(ct map[string]any) {
			ct["Config"].(map[string]any)["Labels"].(map[string]any)["overridden"] = "img"
		}, "Config.Labels.overridden"},
		{"a host setting dropped", func(ct map[string]any) {
			delete(ct["HostConfig"].(map[string]any), "DeviceRequests")
		}, "HostConfig.DeviceRequests"},
		{"a device request changed", func(ct map[string]any) {
			ct["HostConfig"].(map[string]any)["DeviceRequests"] = []any{map[string]any{"Driver": "nvidia", "Count": json.Number("-1")}}
		}, "HostConfig.DeviceRequests[0]"},
		{"a port binding changed", func(ct map[string]any) {
			ct["HostConfig"].(map[string]any)["PortBindings"] = map[string]any{"80/tcp": []any{map[string]any{"HostPort": "8081"}}}
		}, "HostConfig.PortBindings"},
		{"a network missing", func(ct map[string]any) {
			delete(ct["NetworkSettings"].(map[string]any)["Networks"].(map[string]any), "back")
		}, "network back"},
		{"an alias missing", func(ct map[string]any) {
			ct["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)["back"].(map[string]any)["Aliases"] = []any{"x"}
		}, "network back.Aliases"},
		{"a volume replaced", func(ct map[string]any) {
			ms := list(ct["Mounts"])
			m := map[string]any{}
			for k, v := range object(ms[3]) {
				m[k] = v
			}
			m["Name"] = "fresh"
			ct["Mounts"] = append(append([]any{}, ms[:3]...), m)
		}, "mount /anon"},
		{"a mount missing", func(ct map[string]any) { ct["Mounts"] = list(ct["Mounts"])[1:] }, "mount /conf"},
		{"a bind's propagation changed", func(ct map[string]any) {
			ms := list(ct["Mounts"])
			m := map[string]any{}
			for k, v := range object(ms[0]) {
				m[k] = v
			}
			m["Propagation"] = "rshared"
			ct["Mounts"] = append([]any{m}, ms[1:]...)
		}, "mount /conf: propagation"},
		{"an exposed port missing", func(ct map[string]any) {
			delete(ct["Config"].(map[string]any), "ExposedPorts")
		}, "Config.ExposedPorts is missing"},
		{"a volume key missing", func(ct map[string]any) {
			ct["Config"].(map[string]any)["Volumes"] = map[string]any{}
		}, "Config.Volumes./extra is missing"},
	}
	for _, tt := range tests {
		err := compare(s, []byte(oldContainer), newFrom(t, s, oldContainer, tt.edit), newImage)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to name %q", tt.name, err, tt.want)
		}
	}
}

func TestCompareNoLimitPids(t *testing.T) {
	ct := strings.Replace(oldContainer, `"PidsLimit": null`, `"PidsLimit": -1`, 1)
	s := mustBuild(t, ct, oldImageDoc)
	stored := newFrom(t, s, ct, func(c map[string]any) { c["HostConfig"].(map[string]any)["PidsLimit"] = nil })
	if err := compare(s, []byte(ct), stored, newImage); err != nil {
		t.Fatalf("a limit of -1 stored as none: %v", err)
	}
}

// Environment values may be secrets; a failed check names the variable only.
func TestCompareHidesEnvironmentValues(t *testing.T) {
	s := mustBuild(t, oldContainer, oldImageDoc)
	err := compare(s, []byte(oldContainer), newFrom(t, s, oldContainer, func(ct map[string]any) {
		ct["Config"].(map[string]any)["Env"] = []any{"EXTRA=x"}
	}), newImage)
	if err == nil || !strings.Contains(err.Error(), "KEEP") || strings.Contains(err.Error(), "mine") {
		t.Fatalf("err = %v", err)
	}
}

// A label with an empty value is still a label.
func TestCompareEmptyLabel(t *testing.T) {
	ct := strings.Replace(oldContainer, `"overridden": "mine",`, `"overridden": "mine", "feature": "",`, 1)
	s := mustBuild(t, ct, oldImageDoc)
	err := compare(s, []byte(ct), newFrom(t, s, ct, func(c map[string]any) {
		delete(c["Config"].(map[string]any)["Labels"].(map[string]any), "feature")
	}), newImage)
	if err == nil || !strings.Contains(err.Error(), "Config.Labels.feature is missing") {
		t.Fatalf("err = %v", err)
	}
}
