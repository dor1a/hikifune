package update

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	oldID    = "0123456789ab0123456789ab0123456789ab0123456789ab0123456789abcdef"
	oldImage = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newImage = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// oldContainer is an inspect document shaped like what Docker 29 reports
// for a compose service on three networks.
var oldContainer = `{
	"Id": "` + oldID + `",
	"Name": "/apps-web-1",
	"Image": "` + oldImage + `",
	"Config": {
		"Hostname": "0123456789ab",
		"User": "app",
		"Env": ["PATH=/usr/bin:/bin", "IMG=v1", "KEEP=mine", "EXTRA=x"],
		"Cmd": ["serve"],
		"Entrypoint": ["/app"],
		"Healthcheck": {"Test": ["CMD", "/app", "healthcheck"], "Interval": 1000000000},
		"WorkingDir": "/srv",
		"ExposedPorts": {"80/tcp": {}, "9000/tcp": {}},
		"Volumes": {"/anon": {}, "/extra": {}},
		"Labels": {
			"from.image": "1",
			"overridden": "mine",
			"com.docker.compose.project": "apps",
			"com.docker.compose.image": "` + oldImage + `"
		},
		"StopSignal": "SIGTERM",
		"MacAddress": "02:42:ac:11:00:02",
		"Image": "reg.example/team/web:1",
		"Tty": false
	},
	"HostConfig": {
		"NetworkMode": "apps_default",
		"Binds": ["/srv/conf:/conf:ro", "data:/data"],
		"Mounts": [{"Type": "volume", "Target": "/cache"}],
		"PortBindings": {"80/tcp": [{"HostIp": "", "HostPort": "8080"}]},
		"RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
		"Memory": 9007199254740993,
		"PidsLimit": null,
		"DeviceRequests": [{"Driver": "", "Count": -1, "DeviceIDs": null, "Capabilities": [["gpu"]], "Options": {}}],
		"AutoRemove": false
	},
	"Mounts": [
		{"Type": "bind", "Source": "/srv/conf", "Destination": "/conf", "Mode": "ro", "RW": false},
		{"Type": "volume", "Name": "data", "Source": "/var/lib/docker/volumes/data/_data", "Destination": "/data", "RW": true},
		{"Type": "volume", "Name": "cachevol", "Source": "/var/lib/docker/volumes/cachevol/_data", "Destination": "/cache", "RW": true},
		{"Type": "volume", "Name": "anonvol", "Source": "/var/lib/docker/volumes/anonvol/_data", "Destination": "/anon", "RW": true}
	],
	"NetworkSettings": {
		"Networks": {
			"apps_default": {
				"IPAMConfig": null, "Links": null,
				"Aliases": ["apps-web-1", "web", "0123456789ab"],
				"MacAddress": "02:42:ac:11:00:02", "DriverOpts": null, "GwPriority": 0,
				"NetworkID": "n1", "EndpointID": "e1", "Gateway": "172.18.0.1",
				"IPAddress": "172.18.0.2", "IPPrefixLen": 16, "IPv6Gateway": "",
				"GlobalIPv6Address": "", "GlobalIPv6PrefixLen": 0,
				"DNSNames": ["apps-web-1", "web", "0123456789ab"]
			},
			"back": {"IPAMConfig": {"IPv4Address": "172.30.0.9"}, "Aliases": ["` + oldID + `", "api"], "NetworkID": "n2", "IPAddress": "172.30.0.9"},
			"front": {"Aliases": null, "NetworkID": "n3"}
		}
	}
}`

var oldImageDoc = `{
	"Id": "` + oldImage + `",
	"Config": {
		"User": "app",
		"Env": ["PATH=/usr/bin:/bin", "IMG=v1", "KEEP=img"],
		"Cmd": ["serve"],
		"Entrypoint": ["/app"],
		"Healthcheck": {"Test": ["CMD", "/app", "healthcheck"], "Interval": 1000000000},
		"WorkingDir": "/srv",
		"ExposedPorts": {"80/tcp": {}},
		"Volumes": {"/anon": {}},
		"Labels": {"from.image": "1", "overridden": "img"},
		"StopSignal": "SIGTERM"
	}
}`

func mustBuild(t *testing.T, container, image string) spec {
	t.Helper()
	s, err := build([]byte(container), []byte(image), oldImage, newImage)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildDropsWhatCameFromTheImage(t *testing.T) {
	s := mustBuild(t, oldContainer, oldImageDoc)
	cfg := s.config
	if got := jsonOf(t, cfg["Env"]); got != `["KEEP=mine","EXTRA=x"]` {
		t.Errorf("Env = %s", got)
	}
	if got := jsonOf(t, cfg["Labels"]); got != `{"com.docker.compose.image":"`+newImage+`","com.docker.compose.project":"apps","overridden":"mine"}` {
		t.Errorf("Labels = %s", got)
	}
	if got := jsonOf(t, cfg["ExposedPorts"]); got != `{"9000/tcp":{}}` {
		t.Errorf("ExposedPorts = %s", got)
	}
	if got := jsonOf(t, cfg["Volumes"]); got != `{"/extra":{}}` {
		t.Errorf("Volumes = %s", got)
	}
	for _, k := range []string{"Entrypoint", "Cmd", "Healthcheck", "WorkingDir", "User", "StopSignal", "Hostname", "MacAddress"} {
		if v, ok := cfg[k]; ok {
			t.Errorf("%s kept as %s", k, jsonOf(t, v))
		}
	}
	if cfg["Image"] != "reg.example/team/web:1" {
		t.Errorf("Image = %v", cfg["Image"])
	}
	if _, ok := cfg["Tty"]; !ok {
		t.Error("Tty, set on the container, was dropped")
	}
}

func TestBuildEntrypointAndCmd(t *testing.T) {
	tests := []struct {
		name               string
		ctEntry, ctCmd     string
		wantEntry, wantCmd bool
	}{
		{"both from the image", `["/app"]`, `["serve"]`, false, false},
		{"own command", `["/app"]`, `["migrate"]`, false, true},
		{"own entrypoint keeps the command", `["/bin/sh"]`, `["serve"]`, true, true},
	}
	for _, tt := range tests {
		ct := strings.Replace(oldContainer, `"Cmd": ["serve"],
		"Entrypoint": ["/app"],`, `"Cmd": `+tt.ctCmd+`, "Entrypoint": `+tt.ctEntry+`,`, 1)
		if ct == oldContainer && tt.name != "both from the image" {
			t.Fatal("fixture not replaced")
		}
		s := mustBuild(t, ct, oldImageDoc)
		_, entry := s.config["Entrypoint"]
		_, cmd := s.config["Cmd"]
		if entry != tt.wantEntry || cmd != tt.wantCmd {
			t.Errorf("%s: Entrypoint kept %v, Cmd kept %v; want %v, %v", tt.name, entry, cmd, tt.wantEntry, tt.wantCmd)
		}
	}
}

func TestBuildKeepsWhatWasSet(t *testing.T) {
	ct := strings.Replace(oldContainer, `"Hostname": "0123456789ab"`, `"Hostname": "web-host"`, 1)
	ct = strings.Replace(ct, `"Healthcheck": {"Test": ["CMD", "/app", "healthcheck"], "Interval": 1000000000}`, `"Healthcheck": {"Test": ["NONE"]}`, 1)
	s := mustBuild(t, ct, oldImageDoc)
	if s.config["Hostname"] != "web-host" {
		t.Errorf("Hostname = %v", s.config["Hostname"])
	}
	if got := jsonOf(t, s.config["Healthcheck"]); got != `{"Test":["NONE"]}` {
		t.Errorf("Healthcheck = %s", got)
	}

	host := strings.Replace(oldContainer, `"Hostname": "0123456789ab"`, `"Hostname": "the-host"`, 1)
	host = strings.Replace(host, `"NetworkMode": "apps_default"`, `"NetworkMode": "host"`, 1)
	s = mustBuild(t, host, oldImageDoc)
	if _, ok := s.config["Hostname"]; ok {
		t.Error("Hostname kept under the host network mode")
	}
	if s.primary != "" || len(s.connectLast) != 0 {
		t.Errorf("host mode got networks %q %v", s.primary, s.connectLast)
	}
}

func TestBuildHostConfig(t *testing.T) {
	s := mustBuild(t, oldContainer, oldImageDoc)
	hc := s.hostConfig
	// Numbers go back exactly as the daemon wrote them.
	if got := jsonOf(t, hc["Memory"]); got != "9007199254740993" {
		t.Errorf("Memory = %s", got)
	}
	if got := jsonOf(t, hc["DeviceRequests"]); got != `[{"Capabilities":[["gpu"]],"Count":-1,"DeviceIDs":null,"Driver":"","Options":{}}]` {
		t.Errorf("DeviceRequests = %s", got)
	}
	if got := jsonOf(t, hc["Binds"]); got != `["/srv/conf:/conf:ro","data:/data"]` {
		t.Errorf("Binds = %s", got)
	}
	// The compose volume without a source gets the old volume, and the
	// image's anonymous volume is mounted again.
	want := `[{"Source":"cachevol","Target":"/cache","Type":"volume"},{"Source":"anonvol","Target":"/anon","Type":"volume"}]`
	if got := jsonOf(t, hc["Mounts"]); got != want {
		t.Errorf("Mounts = %s\nwant     %s", got, want)
	}
}

func TestBuildNetworks(t *testing.T) {
	s := mustBuild(t, oldContainer, oldImageDoc)
	if s.primary != "apps_default" || !slices.Equal(s.connectLast, []string{"back", "front"}) {
		t.Fatalf("primary %q, then %v", s.primary, s.connectLast)
	}
	if got := jsonOf(t, s.endpoints["apps_default"]); got != `{"Aliases":["apps-web-1","web"],"DriverOpts":null,"GwPriority":0,"IPAMConfig":null,"Links":null}` {
		t.Errorf("apps_default = %s", got)
	}
	if got := jsonOf(t, s.endpoints["back"]); got != `{"Aliases":["api"],"IPAMConfig":{"IPv4Address":"172.30.0.9"}}` {
		t.Errorf("back = %s", got)
	}
	if got := jsonOf(t, s.endpoints["front"]); got != `{}` {
		t.Errorf("front = %s", got)
	}
	body, err := s.body()
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	json.Unmarshal(body, &b)
	ec := b["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if len(ec) != 1 || ec["apps_default"] == nil || b["HostConfig"] == nil || b["Image"] != "reg.example/team/web:1" {
		t.Errorf("body = %s", body)
	}

	// The default network mode names the bridge network.
	def := `{"Id":"` + oldID + `","Config":{"Image":"x"},"HostConfig":{"NetworkMode":"default"},
		"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"b"}}}}`
	if s := mustBuild(t, def, `{}`); s.primary != "bridge" {
		t.Errorf("default mode: primary %q", s.primary)
	}
	bad := `{"Id":"` + oldID + `","Config":{"Image":"x"},"HostConfig":{"NetworkMode":"gone"},
		"NetworkSettings":{"Networks":{"a":{},"b":{}}}}`
	if _, err := build([]byte(bad), []byte(`{}`), oldImage, newImage); err == nil {
		t.Error("a network mode naming none of the networks was accepted")
	}
}

func TestBuildLeavesAnotherComposeImageLabel(t *testing.T) {
	ct := strings.Replace(oldContainer, `"com.docker.compose.image": "`+oldImage+`"`, `"com.docker.compose.image": "sha256:pinned"`, 1)
	s := mustBuild(t, ct, oldImageDoc)
	if got := s.config["Labels"].(map[string]any)[composeImageLabel]; got != "sha256:pinned" {
		t.Errorf("label = %v", got)
	}
}

func TestBuildRefusesBrokenDocuments(t *testing.T) {
	for _, doc := range []string{`null`, `{}`, `{"Id":"x"}`, `not json`} {
		if _, err := build([]byte(doc), []byte(oldImageDoc), oldImage, newImage); err == nil {
			t.Errorf("build(%s) gave no error", doc)
		}
	}
}

func TestSame(t *testing.T) {
	num := func(s string) json.Number { return json.Number(s) }
	tests := []struct {
		a, b any
		want bool
	}{
		{nil, []any{}, true},
		{nil, map[string]any{}, true},
		{"", nil, true},
		{false, nil, true},
		{num("0"), nil, true},
		{[]any{"a"}, []any{"a"}, true},
		{[]any{"a"}, []any{"a", "b"}, false},
		{map[string]any{"a": nil}, map[string]any{}, true},
		{map[string]any{"a": "1"}, map[string]any{"a": "1", "b": "2"}, false},
		{num("1"), num("1"), true},
		{num("1"), "1", false},
		{"x", "y", false},
	}
	for _, tt := range tests {
		if got := same(tt.a, tt.b); got != tt.want {
			t.Errorf("same(%#v, %#v) = %v", tt.a, tt.b, got)
		}
		if got := same(tt.b, tt.a); got != tt.want {
			t.Errorf("same(%#v, %#v) = %v", tt.b, tt.a, got)
		}
	}
	if !reflect.DeepEqual(without([]any{"a", "b"}, []any{"b"}), []any{"a"}) {
		t.Error("without")
	}
}

// An empty entrypoint clears the image's; it is not the same as none.
func TestBuildKeepsAnEmptyEntrypoint(t *testing.T) {
	ct := strings.Replace(oldContainer, `"Cmd": ["serve"],
		"Entrypoint": ["/app"],`, `"Cmd": ["serve"], "Entrypoint": [],`, 1)
	img := strings.Replace(oldImageDoc, `"Entrypoint": ["/app"],`, `"Entrypoint": null,`, 1)
	s := mustBuild(t, ct, img)
	if got := jsonOf(t, s.config["Entrypoint"]); got != `[]` {
		t.Errorf("Entrypoint = %s, want []", got)
	}
	if got := jsonOf(t, s.config["Cmd"]); got != `["serve"]` {
		t.Errorf("Cmd = %s, kept with its own entrypoint", got)
	}
}

// A container created with --network <id> has the ID as its network mode.
func TestBuildPrimaryNetworkByID(t *testing.T) {
	ct := strings.Replace(oldContainer, `"NetworkMode": "apps_default"`, `"NetworkMode": "n2"`, 1)
	s := mustBuild(t, ct, oldImageDoc)
	if s.primary != "back" || s.primaryKey != "n2" || !slices.Equal(s.connectLast, []string{"apps_default", "front"}) {
		t.Fatalf("primary %q as %q, then %v", s.primary, s.primaryKey, s.connectLast)
	}
	body, _ := s.body()
	if !strings.Contains(string(body), `"EndpointsConfig":{"n2":{`) {
		t.Errorf("body = %s", body)
	}
}

// A bind with no source is an anonymous volume; it keeps the old one.
func TestBuildAnonymousVolumeInBinds(t *testing.T) {
	ct := strings.Replace(oldContainer, `"Binds": ["/srv/conf:/conf:ro", "data:/data"]`, `"Binds": ["/srv/conf:/conf:ro", "data:/data", "/anon:ro"]`, 1)
	s := mustBuild(t, ct, oldImageDoc)
	if got := jsonOf(t, s.hostConfig["Binds"]); got != `["/srv/conf:/conf:ro","data:/data","anonvol:/anon:ro"]` {
		t.Errorf("Binds = %s", got)
	}
	if got := jsonOf(t, s.hostConfig["Mounts"]); strings.Contains(got, `"/anon"`) {
		t.Errorf("/anon mounted twice: %s", got)
	}
}
