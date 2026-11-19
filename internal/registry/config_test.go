package registry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParseConfig(t *testing.T) {
	k, err := ParseConfig([]byte(`{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "` + b64("hubuser:hub:pass") + `"},
			"ghcr.io": {"username": "gh", "password": "token"},
			"https://registry.example.com:5000/v2/": {"auth": "` + b64("u:p") + `"},
			"quay.io": {}
		},
		"credHelpers": {"123.dkr.ecr.eu-west-1.amazonaws.com": "ecr-login"},
		"credsStore": "desktop"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for registry, want := range map[string]Credentials{
		"docker.io":                 {"hubuser", "hub:pass"},
		"ghcr.io":                   {"gh", "token"},
		"registry.example.com:5000": {"u", "p"},
	} {
		if got, ok := k.Get(registry); !ok || got != want {
			t.Errorf("Get(%q) = %+v, %v, want %+v", registry, got, ok, want)
		}
	}
	if _, ok := k.Get("quay.io"); ok {
		t.Error("an empty auths entry counts as credentials")
	}
	if h := k.Helper("ghcr.io"); h != "" {
		t.Errorf("Helper(ghcr.io) = %q, want none: auths has it", h)
	}
	if h := k.Helper("123.dkr.ecr.eu-west-1.amazonaws.com"); h != "ecr-login" {
		t.Errorf("Helper(ecr) = %q, want ecr-login", h)
	}
	if h := k.Helper("quay.io"); h != "desktop" {
		t.Errorf("Helper(quay.io) = %q, want the credsStore", h)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for _, in := range []string{
		`{"auths": {"ghcr.io": {"auth": "not base64!"}}}`,
		`{"auths": {"ghcr.io": {"auth": "` + b64("nocolon") + `"}}}`,
		`not json`,
	} {
		if _, err := ParseConfig([]byte(in)); err == nil {
			t.Errorf("ParseConfig(%s) accepted it", in)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	k, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := k.Get("docker.io"); ok || k.Helper("docker.io") != "" {
		t.Error("an empty path gives credentials")
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing file is not an error")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auths": {"ghcr.io": {"auth": "`+b64("a:b")+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	} else if c, ok := k.Get("ghcr.io"); !ok || c.Password != "b" {
		t.Errorf("LoadConfig credentials = %+v, %v", c, ok)
	}
}

func TestParseChallenge(t *testing.T) {
	tests := []struct {
		in     string
		scheme string
		params map[string]string
	}{
		{`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`,
			"bearer", map[string]string{"realm": "https://auth.docker.io/token", "service": "registry.docker.io", "scope": "repository:library/alpine:pull"}},
		{`Bearer realm="https://ghcr.io/token", service="ghcr.io", scope="repository:a/b:pull,push"`,
			"bearer", map[string]string{"realm": "https://ghcr.io/token", "service": "ghcr.io", "scope": "repository:a/b:pull,push"}},
		{`Basic realm="Registry \"Realm\""`, "basic", map[string]string{"realm": `Registry "Realm"`}},
		{`basic realm=plain, charset="UTF-8"`, "basic", map[string]string{"realm": "plain", "charset": "UTF-8"}},
	}
	for _, tt := range tests {
		ch, ok := parseChallenge(tt.in)
		if !ok || ch.scheme != tt.scheme || len(ch.params) != len(tt.params) {
			t.Errorf("parseChallenge(%s) = %+v", tt.in, ch)
			continue
		}
		for k, v := range tt.params {
			if ch.params[k] != v {
				t.Errorf("parseChallenge(%s)[%s] = %q, want %q", tt.in, k, ch.params[k], v)
			}
		}
	}
}

func TestParseConfigPrefersTheDockerLoginKey(t *testing.T) {
	in := []byte(`{"auths": {
		"docker.io": {"auth": "` + b64("old:x") + `"},
		"https://index.docker.io/v1/": {"auth": "` + b64("new:y") + `"},
		"index.docker.io": {"auth": "` + b64("mid:z") + `"}
	}}`)
	for range 20 {
		k, err := ParseConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := k.Get("docker.io"); c.Username != "new" {
			t.Fatalf("Docker Hub credentials = %+v, want the https://index.docker.io/v1/ entry", c)
		}
	}
}

func TestParseConfigPrefersTheBareHostKey(t *testing.T) {
	k, err := ParseConfig([]byte(`{"auths": {
		"https://z.example": {"auth": "` + b64("old:x") + `"},
		"z.example": {"auth": "` + b64("new:y") + `"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := k.Get("z.example"); c.Username != "new" {
		t.Errorf("credentials = %+v, want the bare host key", c)
	}
}

func TestPullAuth(t *testing.T) {
	k, err := ParseConfig([]byte(`{"auths":{
		"https://index.docker.io/v1/":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("hub:pw1")) + `"},
		"ghcr.io":{"username":"gh","password":"pw2"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	decode := func(s string) map[string]string {
		t.Helper()
		b, err := base64.URLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("not base64url: %q", s)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := decode(k.PullAuth("docker.io")); m["username"] != "hub" || m["password"] != "pw1" || m["serveraddress"] != "https://index.docker.io/v1/" {
		t.Errorf("docker.io: %v", m)
	}
	if m := decode(k.PullAuth("ghcr.io")); m["username"] != "gh" || m["password"] != "pw2" || m["serveraddress"] != "ghcr.io" {
		t.Errorf("ghcr.io: %v", m)
	}
	if got := k.PullAuth("quay.io"); got != "" {
		t.Errorf("quay.io: %q, want anonymous", got)
	}
	var none *Keychain
	if got := none.PullAuth("ghcr.io"); got != "" {
		t.Errorf("nil keychain: %q", got)
	}
}
