package main

import (
	"strings"
	"testing"
)

func getter(m map[string]string) func(string, string) string {
	return func(k, def string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return def
	}
}

func TestLoadAuth(t *testing.T) {
	ok := map[string]string{
		"HIKIFUNE_AUTH": "oidc", "HIKIFUNE_PUBLIC_URL": "https://hikifune.example.com/",
		"HIKIFUNE_OIDC_ISSUER": "https://id.example.com/application/o/hikifune/", "HIKIFUNE_OIDC_CLIENT_ID": "hikifune",
		"HIKIFUNE_OIDC_CLIENT_SECRET": "s", "HIKIFUNE_SESSION_KEY": strings.Repeat("k", 32),
	}
	s, err := loadAuth(getter(ok))
	if err != nil || s == nil || s.PublicURL != "https://hikifune.example.com" || s.Group != "hikifune-users" || s.GroupsClaim != "groups" {
		t.Fatalf("settings %+v, %v", s, err)
	}
	if s, err := loadAuth(getter(map[string]string{"HIKIFUNE_AUTH": " None "})); s != nil || err != nil {
		t.Fatalf("none: %+v, %v", s, err)
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for _, tt := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "HIKIFUNE_AUTH is required"},
		{map[string]string{"HIKIFUNE_AUTH": "basic"}, `got "basic"`},
		{with("HIKIFUNE_OIDC_ISSUER", ""), "HIKIFUNE_OIDC_ISSUER is required"},
		{with("HIKIFUNE_SESSION_KEY", "short"), "at least 32 bytes"},
		{with("HIKIFUNE_PUBLIC_URL", "https://example.com/hikifune"), "no path"},
		{with("HIKIFUNE_PUBLIC_URL", "ftp://example.com"), "no path"},
		{with("HIKIFUNE_OIDC_SCOPES", "profile email"), "must include openid"},
	} {
		if _, err := loadAuth(getter(tt.env)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: err %v, want %q", tt.env, err, tt.want)
		}
	}
}

func TestLoadGravatar(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "true": true, " TRUE ": true, "1": true, "0": false} {
		m := map[string]string{}
		if v != "" {
			m["HIKIFUNE_GRAVATAR"] = v
		}
		if got, err := loadGravatar(getter(m)); err != nil || got != want {
			t.Errorf("%q: %v, %v", v, got, err)
		}
	}
	if _, err := loadGravatar(getter(map[string]string{"HIKIFUNE_GRAVATAR": "yes"})); err == nil || !strings.Contains(err.Error(), `got "yes"`) {
		t.Errorf("yes: %v", err)
	}
}

func TestAgentName(t *testing.T) {
	for _, ok := range []string{"atsui", "nas-1", "rack.a_2", strings.Repeat("a", 63)} {
		if !agentName.MatchString(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "rack/a", "-a", ".a", "a b", "a%2F", strings.Repeat("a", 64)} {
		if agentName.MatchString(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}
