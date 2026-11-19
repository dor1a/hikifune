package main

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/dor1a/hikifune/internal/auth"
)

// Auth modes in HIKIFUNE_AUTH (R33).
const (
	authOIDC = "oidc"
	authNone = "none"
)

// loadAuth reads the sign-in settings. HIKIFUNE_AUTH has no default, so the
// server never starts open by accident; none returns nil settings and leaves
// access control to a proxy in front.
func loadAuth(get func(key, def string) string) (*auth.Settings, error) {
	switch mode := strings.ToLower(strings.TrimSpace(get("HIKIFUNE_AUTH", ""))); mode {
	case authNone:
		return nil, nil
	case authOIDC:
	case "":
		return nil, fmt.Errorf("HIKIFUNE_AUTH is required: set it to %q or %q", authOIDC, authNone)
	default:
		return nil, fmt.Errorf("HIKIFUNE_AUTH must be %q or %q, got %q", authOIDC, authNone, mode)
	}
	s := auth.Settings{
		PublicURL:    strings.TrimRight(get("HIKIFUNE_PUBLIC_URL", ""), "/"),
		Issuer:       get("HIKIFUNE_OIDC_ISSUER", ""),
		ClientID:     get("HIKIFUNE_OIDC_CLIENT_ID", ""),
		ClientSecret: get("HIKIFUNE_OIDC_CLIENT_SECRET", ""),
		Group:        get("HIKIFUNE_OIDC_ALLOWED_GROUP", "hikifune-users"),
		GroupsClaim:  get("HIKIFUNE_OIDC_GROUPS_CLAIM", "groups"),
		Scopes:       strings.Fields(get("HIKIFUNE_OIDC_SCOPES", "openid profile email")),
		SessionKey:   []byte(get("HIKIFUNE_SESSION_KEY", "")),
	}
	var errs []error
	for _, v := range []struct{ key, value string }{
		{"HIKIFUNE_PUBLIC_URL", s.PublicURL},
		{"HIKIFUNE_OIDC_ISSUER", s.Issuer},
		{"HIKIFUNE_OIDC_CLIENT_ID", s.ClientID},
		{"HIKIFUNE_OIDC_CLIENT_SECRET", s.ClientSecret},
		{"HIKIFUNE_SESSION_KEY", string(s.SessionKey)},
	} {
		if v.value == "" {
			errs = append(errs, fmt.Errorf("%s is required when HIKIFUNE_AUTH=%s", v.key, authOIDC))
		}
	}
	if n := len(s.SessionKey); n > 0 && n < 32 {
		errs = append(errs, fmt.Errorf("HIKIFUNE_SESSION_KEY must be at least 32 bytes, got %d", n))
	}
	if s.PublicURL != "" {
		// The server serves from the root, so the URL can have no path.
		u, err := url.Parse(s.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("HIKIFUNE_PUBLIC_URL must be an http or https address with no path, such as https://hikifune.example.com, got %q", s.PublicURL))
		}
	}
	if !slices.Contains(s.Scopes, "openid") {
		errs = append(errs, fmt.Errorf("HIKIFUNE_OIDC_SCOPES must include openid, got %q", strings.Join(s.Scopes, " ")))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &s, nil
}
