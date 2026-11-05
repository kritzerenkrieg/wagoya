package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
)

// Keys are stored API credentials (I4): request headers per host, added by
// http_request for whitelisted requesters only. The model never sees them.
// The file maps a host to its headers:
//
//	{"tasks.example.com": {"Authorization": "Bearer <token>"}}
type Keys map[string]map[string]string

// LoadKeys reads the keys file. A missing file means no keys.
func LoadKeys(path string) (Keys, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var k Keys
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := Keys{}
	for host, hs := range k {
		h := strings.ToLower(strings.TrimSpace(host))
		if h == "" || strings.ContainsAny(h, "/:") {
			return nil, fmt.Errorf("%s: %q is not a bare host name (like tasks.example.com)", path, host)
		}
		for name, v := range hs {
			if strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("%s: empty %s for %s", path, name, h)
			}
		}
		out[h] = hs
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		log.Printf("warning: %s is readable by other users; chmod 600 it", path)
	}
	return out, nil
}

// Hosts lists the hosts that have keys, sorted.
func (k Keys) Hosts() []string {
	out := make([]string, 0, len(k))
	for h := range k {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// apply adds the host's stored headers to req (https only) and returns the
// header names it set, or nil.
func (k Keys) apply(req *http.Request) []string {
	hs := k[strings.ToLower(req.URL.Hostname())]
	if hs == nil || req.URL.Scheme != "https" {
		return nil
	}
	var names []string
	for name, v := range hs {
		req.Header.Set(name, v)
		names = append(names, name)
	}
	return names
}

// scrub replaces every stored secret in s, so a response or error that
// echoes a credential back doesn't reach the model.
func (k Keys) scrub(s string) string {
	for _, hs := range k {
		for _, v := range hs {
			s = strings.ReplaceAll(s, v, "[stored credential]")
			if _, tok, ok := strings.Cut(v, " "); ok && len(tok) >= 8 { // "Bearer <token>"
				s = strings.ReplaceAll(s, tok, "[stored credential]")
			}
		}
	}
	return s
}
