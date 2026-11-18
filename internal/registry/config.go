package registry

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
)

// dockerHubConfigKey is the auths key docker login writes for Docker Hub.
const dockerHubConfigKey = "https://index.docker.io/v1/"

// Credentials are a user name and a password or access token for one registry.
type Credentials struct {
	Username string
	Password string
}

// Keychain holds the registry credentials of a Docker config.json. Only the
// auths section is read; credential helpers are only noted, so a failed
// check can say where the credentials are.
type Keychain struct {
	auths   map[string]Credentials
	helpers map[string]string // registry -> helper name from credHelpers
	store   string            // credsStore, the helper for every other registry
}

// LoadConfig reads a Docker config.json. An empty path gives an empty
// keychain, so every registry is asked anonymously.
func LoadConfig(path string) (*Keychain, error) {
	if path == "" {
		return &Keychain{}, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("docker config %s does not exist", path)
	}
	if err != nil {
		return nil, fmt.Errorf("docker config: %w", err)
	}
	k, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("docker config %s: %w", path, err)
	}
	return k, nil
}

// ParseConfig reads the auths, credHelpers and credsStore of a config.json.
// An auths entry gives its credentials either as auth, the base64 of
// user:password that docker login writes, or as username and password.
func ParseConfig(b []byte) (*Keychain, error) {
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
		CredHelpers map[string]string `json:"credHelpers"`
		CredsStore  string            `json:"credsStore"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	k := &Keychain{auths: map[string]Credentials{}, helpers: map[string]string{}, store: cfg.CredsStore}
	// Two keys can name one registry (ghcr.io and https://ghcr.io). Later
	// keys win: URL forms first, then the bare host docker login writes, and
	// for Docker Hub the https://index.docker.io/v1/ key last, so the same
	// file always gives the same credentials.
	keys := slices.Collect(maps.Keys(cfg.Auths))
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(keyRank(a), keyRank(b)), strings.Compare(a, b))
	})
	for _, key := range keys {
		a := cfg.Auths[key]
		c := Credentials{Username: a.Username, Password: a.Password}
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, fmt.Errorf("auths %q: auth is not base64", key)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf("auths %q: auth is not user:password", key)
			}
			c = Credentials{Username: user, Password: pass}
		}
		if c.Username == "" && c.Password == "" {
			// docker login with a credential helper leaves an empty entry here.
			continue
		}
		k.auths[registryKey(key)] = c
	}
	for key, helper := range cfg.CredHelpers {
		k.helpers[registryKey(key)] = helper
	}
	return k, nil
}

// Get returns the credentials for a registry, if the auths section has them.
func (k *Keychain) Get(registry string) (Credentials, bool) {
	c, ok := k.auths[registryKey(registry)]
	return c, ok
}

// Helper names the credential helper that keeps a registry's credentials
// when the auths section has none, or "" when there is none.
func (k *Keychain) Helper(registry string) string {
	key := registryKey(registry)
	if _, ok := k.auths[key]; ok {
		return ""
	}
	if h := k.helpers[key]; h != "" {
		return h
	}
	return k.store
}

// keyRank orders the keys that can name one registry; a higher rank wins.
func keyRank(key string) int {
	switch {
	case key == dockerHubConfigKey:
		return 2
	case registryKey(key) == key:
		return 1
	}
	return 0
}

// registryKey turns a config.json key into a registry name: docker login
// writes https://index.docker.io/v1/ for Docker Hub and a bare host or a
// URL for the rest.
func registryKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	key, _, _ = strings.Cut(key, "/")
	switch key {
	case "index.docker.io", dockerHubAPI:
		return DockerHub
	}
	return key
}
