// Package guard sits between the hub and the Docker socket proxy. The proxy
// filters by endpoint only; the guard reads every request the hub may send
// and lets through only what the hub itself would ever ask for, so a
// compromised hub cannot start a privileged container or mount the host.
package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Policy is what a create may contain. Lists match exactly.
type Policy struct {
	Images       []string // pinned refs bots and replays run
	BindPrefixes []string // host paths a bind may start with, e.g. /var/lib/wickfleet/
	Networks     []string // docker networks a container may join
	Users        []string // uid:gid a container may run as
	NamePrefix   string   // container names start with this
	ManagedLabel string   // start and delete only touch containers with this label
}

// Allowed request keys, lower case: Docker decodes JSON case-insensitively.
var (
	topKeys  = set("image", "entrypoint", "cmd", "user", "labels", "healthcheck", "hostconfig", "networkingconfig", "exposedports")
	hostKeys = set("binds", "memory", "cpushares", "restartpolicy", "networkmode", "privileged", "readonlyrootfs", "capdrop", "securityopt")
	restarts = set("no", "unless-stopped", "always", "on-failure")
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
	verRe    = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)
	idRe     = regexp.MustCompile(`^/containers/([A-Za-z0-9_.-]+)(/[a-z]+)?$`)
)

func set(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// lower folds an object's keys; a key given twice in any casing is refused,
// since Docker would take one of them and we cannot know which.
func lower(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for k, v := range in {
		lk := strings.ToLower(k)
		if _, dup := out[lk]; dup {
			return nil, fmt.Errorf("key %q given twice", lk)
		}
		out[lk] = v
	}
	return out, nil
}

func only(m map[string]json.RawMessage, allowed map[string]bool, where string) error {
	for k := range m {
		if !allowed[k] {
			return fmt.Errorf("%s.%s is not allowed", where, k)
		}
	}
	return nil
}

func str(raw json.RawMessage) (string, error) {
	var s string
	return s, json.Unmarshal(raw, &s)
}

func strs(raw json.RawMessage) ([]string, error) {
	var s []string
	if string(raw) == "null" {
		return nil, nil
	}
	return s, json.Unmarshal(raw, &s)
}

// CheckCreate judges a POST /containers/create body and its name.
func (p Policy) CheckCreate(name string, body []byte) error {
	if !strings.HasPrefix(name, p.NamePrefix) || !nameRe.MatchString(name) {
		return fmt.Errorf("container name %q is not allowed", name)
	}
	top, err := lower(body)
	if err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if err := only(top, topKeys, "create"); err != nil {
		return err
	}
	img, err := str(top["image"])
	if err != nil || !slices.Contains(p.Images, img) {
		return fmt.Errorf("image %q is not allowed", img)
	}
	user, err := str(top["user"])
	if err != nil || !slices.Contains(p.Users, user) {
		return fmt.Errorf("user %q is not allowed", user)
	}
	if raw, ok := top["labels"]; ok {
		var labels map[string]string
		if err := json.Unmarshal(raw, &labels); err != nil {
			return fmt.Errorf("labels: %w", err)
		}
		if _, ok := labels[p.ManagedLabel]; !ok {
			return fmt.Errorf("label %s is required", p.ManagedLabel)
		}
	} else {
		return fmt.Errorf("label %s is required", p.ManagedLabel)
	}
	if err := p.checkHost(top["hostconfig"]); err != nil {
		return err
	}
	return p.checkNetworking(top["networkingconfig"])
}

func (p Policy) checkHost(raw json.RawMessage) error {
	if raw == nil {
		return errors.New("hostconfig is required")
	}
	hc, err := lower(raw)
	if err != nil {
		return fmt.Errorf("hostconfig: %w", err)
	}
	if err := only(hc, hostKeys, "hostconfig"); err != nil {
		return err
	}
	if v, ok := hc["privileged"]; ok && string(v) != "false" {
		return errors.New("privileged containers are not allowed")
	}
	// Lower weight only: above Docker's 1024 a container would outrank the bots.
	if v, ok := hc["cpushares"]; ok {
		var n int64
		if err := json.Unmarshal(v, &n); err != nil || n < 2 || n > 1024 {
			return errors.New("hostconfig.cpushares must be a whole number from 2 to 1024")
		}
	}
	if v, ok := hc["readonlyrootfs"]; ok && string(v) != "false" && string(v) != "true" {
		return errors.New("hostconfig.readonlyrootfs must be a bool")
	}
	drop, err := strs(hc["capdrop"])
	if err != nil || !slices.Contains(drop, "ALL") {
		return errors.New("capdrop must include ALL")
	}
	sec, err := strs(hc["securityopt"])
	if err != nil || len(sec) != 1 || sec[0] != "no-new-privileges:true" {
		return errors.New("securityopt must be exactly no-new-privileges:true")
	}
	mode, err := str(hc["networkmode"])
	if err != nil || !slices.Contains(p.Networks, mode) {
		return fmt.Errorf("network mode %q is not allowed", mode)
	}
	binds, err := strs(hc["binds"])
	if err != nil {
		return fmt.Errorf("binds: %w", err)
	}
	for _, b := range binds {
		if err := p.checkBind(b); err != nil {
			return err
		}
	}
	if raw, ok := hc["restartpolicy"]; ok {
		rp, err := lower(raw)
		if err != nil {
			return fmt.Errorf("restartpolicy: %w", err)
		}
		if err := only(rp, set("name"), "restartpolicy"); err != nil {
			return err
		}
		if n, err := str(rp["name"]); err != nil || !restarts[n] {
			return fmt.Errorf("restart policy %q is not allowed", n)
		}
	}
	return nil
}

// checkBind takes "src:dst[:opts]"; src must be a clean absolute path under
// an allowed prefix. A named volume (no slash) is refused too.
func (p Policy) checkBind(b string) error {
	parts := strings.Split(b, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("bind %q is malformed", b)
	}
	src := parts[0]
	if !strings.HasPrefix(src, "/") || path.Clean(src) != strings.TrimSuffix(src, "/") || strings.Contains(src, "..") {
		return fmt.Errorf("bind source %q is not a clean absolute path", src)
	}
	for _, pre := range p.BindPrefixes {
		if strings.HasPrefix(src+"/", strings.TrimSuffix(pre, "/")+"/") {
			if len(parts) == 3 && !regexp.MustCompile(`^(ro|rw|z|Z)(,(ro|rw|z|Z))*$`).MatchString(parts[2]) {
				return fmt.Errorf("bind options %q are not allowed", parts[2])
			}
			return nil
		}
	}
	return fmt.Errorf("bind source %q is outside the allowed paths", src)
}

func (p Policy) checkNetworking(raw json.RawMessage) error {
	if raw == nil || string(raw) == "null" {
		return nil
	}
	nc, err := lower(raw)
	if err != nil {
		return fmt.Errorf("networkingconfig: %w", err)
	}
	if err := only(nc, set("endpointsconfig"), "networkingconfig"); err != nil {
		return err
	}
	var eps map[string]json.RawMessage
	if err := json.Unmarshal(nc["endpointsconfig"], &eps); err != nil {
		return fmt.Errorf("endpointsconfig: %w", err)
	}
	for net, ep := range eps {
		if !slices.Contains(p.Networks, net) {
			return fmt.Errorf("network %q is not allowed", net)
		}
		e, err := lower(ep)
		if err != nil {
			return fmt.Errorf("endpoint %s: %w", net, err)
		}
		if err := only(e, set("aliases"), "endpoint"); err != nil {
			return err
		}
	}
	return nil
}

// CheckPull judges POST /images/create: only allowed refs may be pulled.
func (p Policy) CheckPull(q url.Values) error {
	ref := q.Get("fromImage")
	if tag := q.Get("tag"); tag != "" {
		if strings.HasPrefix(tag, "sha256:") {
			ref += "@" + tag
		} else {
			ref += ":" + tag
		}
	}
	if !slices.Contains(p.Images, ref) {
		return fmt.Errorf("pull of %q is not allowed", ref)
	}
	return nil
}

// Guard proxies allowed requests to the socket proxy and refuses the rest.
type Guard struct {
	Policy   Policy
	Upstream *url.URL
	Client   *http.Client // for the label check; nil = default
	Log      func(format string, args ...any)
	proxy    *httputil.ReverseProxy
}

func (g *Guard) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log(format, args...)
	}
}

const maxBody = 1 << 20

func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		io.WriteString(w, "ok\n")
		return
	}
	if g.proxy == nil {
		g.proxy = httputil.NewSingleHostReverseProxy(g.Upstream)
	}
	if err := g.check(r); err != nil {
		g.logf("refused %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "wickfleet-dockerguard: "+err.Error(), http.StatusForbidden)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

func (g *Guard) check(r *http.Request) error {
	p := verRe.ReplaceAllString(r.URL.Path, "")
	switch {
	case r.Method == http.MethodGet && (p == "/containers/json" || p == "/_ping" || p == "/version"):
		return nil
	case r.Method == http.MethodGet:
		if m := idRe.FindStringSubmatch(p); m != nil && (m[2] == "/json" || m[2] == "/logs") {
			return nil
		}
	case r.Method == http.MethodPost && p == "/containers/create":
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			return err
		}
		if len(body) > maxBody {
			return errors.New("body too large")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		return g.Policy.CheckCreate(r.URL.Query().Get("name"), body)
	case r.Method == http.MethodPost && p == "/images/create":
		return g.Policy.CheckPull(r.URL.Query())
	case r.Method == http.MethodPost:
		if m := idRe.FindStringSubmatch(p); m != nil && m[2] == "/start" {
			return g.managed(r, m[1])
		}
	case r.Method == http.MethodDelete:
		if m := idRe.FindStringSubmatch(p); m != nil && m[2] == "" {
			return g.managed(r, m[1])
		}
	}
	return errors.New("endpoint is not allowed")
}

// managed asks Docker (through the socket proxy) whether the container
// carries the managed label before it is started or deleted.
func (g *Guard) managed(r *http.Request, id string) error {
	u := *g.Upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + "/containers/" + url.PathEscape(id) + "/json"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	c := g.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // nothing to protect; Docker answers 404 itself
	}
	var info struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&info); err != nil {
		return err
	}
	if _, ok := info.Config.Labels[g.Policy.ManagedLabel]; !ok {
		return fmt.Errorf("container %s is not managed by wickfleet", id)
	}
	return nil
}
