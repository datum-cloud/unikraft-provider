package imageconvert

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/distribution/reference"
	core "k8s.io/api/core/v1"
)

// Credentials authenticate to a registry. The zero value is anonymous.
type Credentials struct {
	Username string
	Password string
}

// Keyring matches a Pod's pull Secrets to images like the kubelet: by host,
// including wildcards like *.example.com, and repository path prefix, most
// specific first, trying every match. go-containerregistry's Kubernetes
// keychain tries only the first match.
type Keyring struct {
	entries []keyringEntry
}

type keyringEntry struct {
	host, path string
	creds      Credentials
}

// dockerHubHosts all name Docker Hub in credential keys.
var dockerHubHosts = []string{"docker.io", "index.docker.io", "registry-1.docker.io"}

// AddSecret adds a kubernetes.io/dockerconfigjson or kubernetes.io/dockercfg
// Secret's credentials.
func (k *Keyring) AddSecret(secret *core.Secret) error {
	var auths map[string]dockerAuth
	var err error
	switch secret.Type {
	case core.SecretTypeDockerConfigJson:
		var cfg struct {
			Auths map[string]dockerAuth `json:"auths"`
		}
		err = json.Unmarshal(secret.Data[core.DockerConfigJsonKey], &cfg)
		auths = cfg.Auths
	case core.SecretTypeDockercfg:
		err = json.Unmarshal(secret.Data[core.DockerConfigKey], &auths)
	default:
		err = fmt.Errorf("type %s is not a pull secret type", secret.Type)
	}
	if err != nil {
		return fmt.Errorf("pull secret %q: %w", secret.Name, err)
	}
	for key, auth := range auths {
		creds, err := auth.credentials()
		if err != nil {
			return fmt.Errorf("pull secret %q, registry %s: %w", secret.Name, key, err)
		}
		host, path := splitRegistryKey(key)
		k.entries = append(k.entries, keyringEntry{host: host, path: path, creds: creds})
	}
	return nil
}

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

func (a dockerAuth) credentials() (Credentials, error) {
	if a.Auth == "" {
		return Credentials{a.Username, a.Password}, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(a.Auth)
	if err != nil {
		return Credentials{}, err
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return Credentials{}, fmt.Errorf("auth is not user:password")
	}
	return Credentials{user, pass}, nil
}

// splitRegistryKey splits a credential key such as https://index.docker.io/v1/
// or ghcr.io/acme into a host and a repository path prefix.
func splitRegistryKey(key string) (host, path string) {
	if u, err := url.Parse(key); err == nil && u.Host != "" {
		key = u.Host + u.Path
	}
	host, path, _ = strings.Cut(strings.Trim(key, "/"), "/")
	if host == "index.docker.io" && (path == "v1" || path == "v2") {
		path = ""
	}
	return strings.ToLower(host), path
}

// For returns the credentials to try for an image, most specific first, then
// anonymous.
func (k *Keyring) For(named reference.Named) []Credentials {
	host, repo := reference.Domain(named), reference.Path(named)
	var matches []keyringEntry
	for _, e := range k.entries {
		if hostMatches(e.host, host) && (e.path == "" || repo == e.path || strings.HasPrefix(repo, e.path+"/")) {
			matches = append(matches, e)
		}
	}
	// Exact hosts before wildcards, then longer paths first.
	slices.SortStableFunc(matches, func(a, b keyringEntry) int {
		if wa, wb := strings.HasPrefix(a.host, "*."), strings.HasPrefix(b.host, "*."); wa != wb {
			if wa {
				return 1
			}
			return -1
		}
		return len(b.path) - len(a.path)
	})
	var out []Credentials
	for _, m := range matches {
		if !slices.Contains(out, m.creds) {
			out = append(out, m.creds)
		}
	}
	return append(out, Credentials{})
}

func hostMatches(pattern, host string) bool {
	if slices.Contains(dockerHubHosts, pattern) && slices.Contains(dockerHubHosts, host) {
		return true
	}
	suffix, ok := strings.CutPrefix(pattern, "*.")
	if !ok {
		return pattern == host
	}
	// A wildcard matches one label: *.example.com matches a.example.com.
	_, rest, ok := strings.Cut(host, ".")
	return ok && rest == suffix
}
