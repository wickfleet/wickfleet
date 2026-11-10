package guard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var pol = Policy{
	Images:       []string{"ft:1@sha256:aa"},
	BindPrefixes: []string{"/var/lib/wickfleet/bots/", "/var/lib/wickfleet/state/"},
	Networks:     []string{"wickfleet_edge"},
	Users:        []string{"1000:1000"},
	NamePrefix:   "wickfleet-",
	ManagedLabel: "wickfleet.managed",
}

// good is what the hub sends; each attack edits one thing.
func good() map[string]any {
	return map[string]any{
		"Image":  "ft:1@sha256:aa",
		"Cmd":    []string{"trade"},
		"User":   "1000:1000",
		"Labels": map[string]string{"wickfleet.managed": "bot"},
		"HostConfig": map[string]any{
			"Binds":          []string{"/var/lib/wickfleet/state/bots/x/config.json:/freqtrade/user_data/config.json:ro"},
			"Memory":         805306368,
			"RestartPolicy":  map[string]any{"Name": "unless-stopped"},
			"NetworkMode":    "wickfleet_edge",
			"Privileged":     false,
			"ReadonlyRootfs": false,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"wickfleet_edge": map[string]any{"Aliases": []string{"bot-x"}}}},
	}
}

func body(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func host(m map[string]any) map[string]any { return m["HostConfig"].(map[string]any) }

func TestCreateAllowsWhatTheHubSends(t *testing.T) {
	if err := pol.CheckCreate("wickfleet-bot-x", body(t, good())); err != nil {
		t.Fatal(err)
	}
	low := good()
	host(low)["CpuShares"] = 2 // a practice run
	if err := pol.CheckCreate("wickfleet-bot-x", body(t, low)); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRefusesAttacks(t *testing.T) {
	attacks := map[string]func(m map[string]any){
		"privileged":           func(m map[string]any) { host(m)["Privileged"] = true },
		"privileged lowercase": func(m map[string]any) { host(m)["privileged"] = true },
		"capadd":               func(m map[string]any) { host(m)["CapAdd"] = []string{"SYS_ADMIN"} },
		"devices":              func(m map[string]any) { host(m)["Devices"] = []map[string]string{{"PathOnHost": "/dev/sda"}} },
		"pid host":             func(m map[string]any) { host(m)["PidMode"] = "host" },
		"mounts":               func(m map[string]any) { host(m)["Mounts"] = []map[string]string{{"Type": "bind", "Source": "/"}} },
		"network host":         func(m map[string]any) { host(m)["NetworkMode"] = "host" },
		"bind root":            func(m map[string]any) { host(m)["Binds"] = []string{"/:/host"} },
		"bind docker socket":   func(m map[string]any) { host(m)["Binds"] = []string{"/var/run/docker.sock:/s"} },
		"bind escape":          func(m map[string]any) { host(m)["Binds"] = []string{"/var/lib/wickfleet/state/../../../etc:/e"} },
		"bind sibling prefix":  func(m map[string]any) { host(m)["Binds"] = []string{"/var/lib/wickfleet/statex:/e"} },
		"bind named volume":    func(m map[string]any) { host(m)["Binds"] = []string{"data:/d"} },
		"bind odd options":     func(m map[string]any) { host(m)["Binds"] = []string{"/var/lib/wickfleet/state/a:/a:rshared"} },
		"no capdrop":           func(m map[string]any) { delete(host(m), "CapDrop") },
		"seccomp unconfined": func(m map[string]any) {
			host(m)["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=unconfined"}
		},
		"no hostconfig":    func(m map[string]any) { delete(m, "HostConfig") },
		"image":            func(m map[string]any) { m["Image"] = "alpine" },
		"root user":        func(m map[string]any) { m["User"] = "0" },
		"no user":          func(m map[string]any) { delete(m, "User") },
		"no managed label": func(m map[string]any) { m["Labels"] = map[string]string{} },
		"unknown top key":  func(m map[string]any) { m["Volumes"] = map[string]any{"/x": struct{}{}} },
		"foreign network": func(m map[string]any) {
			m["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{"bridge": map[string]any{}}}
		},
		"cpushares above default": func(m map[string]any) { host(m)["CpuShares"] = 2048 },
		"cpushares zero":          func(m map[string]any) { host(m)["CpuShares"] = 0 },
		"cpushares text":          func(m map[string]any) { host(m)["CpuShares"] = "2" },
		"restart weird": func(m map[string]any) {
			host(m)["RestartPolicy"] = map[string]any{"Name": "always", "MaximumRetryCount": 1}
		},
	}
	for name, edit := range attacks {
		m := good()
		edit(m)
		if err := pol.CheckCreate("wickfleet-bot-x", body(t, m)); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	// Two spellings of one key: Docker would pick one, so refuse both.
	dup := `{"Image":"ft:1@sha256:aa","User":"1000:1000","Labels":{"wickfleet.managed":"b"},"HostConfig":{"Privileged":false,"privileged":true,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true"],"NetworkMode":"wickfleet_edge"}}`
	if err := pol.CheckCreate("wickfleet-bot-x", []byte(dup)); err == nil {
		t.Error("duplicate key in two casings: allowed")
	}
	if err := pol.CheckCreate("traefik", body(t, good())); err == nil {
		t.Error("foreign container name: allowed")
	}
}

func TestPull(t *testing.T) {
	if pol.CheckPull(url.Values{"fromImage": {"ft:1@sha256:aa"}}) != nil {
		t.Fatal("pinned ref must pull")
	}
	if pol.CheckPull(url.Values{"fromImage": {"ft"}, "tag": {"sha256:aa"}}) == nil {
		t.Fatal("ref split differently than pinned must not pull")
	}
	if pol.CheckPull(url.Values{"fromImage": {"evil/miner"}}) == nil {
		t.Fatal("unknown image pulled")
	}
}

// upstream plays the socket proxy: it records what reached it.
func upstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/") {
			labels := map[string]string{}
			if strings.Contains(r.URL.Path, "/containers/mine/") {
				labels["wickfleet.managed"] = "bot"
			}
			if strings.Contains(r.URL.Path, "/containers/gone/") {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": labels}})
			if !strings.Contains(r.URL.Path, "/v1.43/") {
				return // the guard's own label lookup
			}
		}
		seen = append(seen, r.Method+" "+r.URL.Path)
		io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestGuardProxiesOnlyAllowed(t *testing.T) {
	up, seen := upstream(t)
	u, _ := url.Parse(up.URL)
	g := httptest.NewServer(&Guard{Policy: pol, Upstream: u})
	defer g.Close()
	do := func(method, path, b string) int {
		req, _ := http.NewRequest(method, g.URL+path, strings.NewReader(b))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	good, _ := json.Marshal(good())
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/healthz", "", 200},
		{"GET", "/v1.43/containers/json?all=1", "", 200},
		{"GET", "/v1.43/containers/mine/logs?tail=50", "", 200},
		{"POST", "/v1.43/containers/create?name=wickfleet-bot-x", string(good), 200},
		{"POST", "/v1.43/containers/create?name=wickfleet-bot-x", `{"Image":"alpine"}`, 403},
		{"POST", "/v1.43/containers/mine/start", "", 200},
		{"POST", "/v1.43/containers/theirs/start", "", 403},
		{"DELETE", "/v1.43/containers/mine?force=1", "", 200},
		{"DELETE", "/v1.43/containers/theirs?force=1", "", 403},
		{"DELETE", "/v1.43/containers/gone?force=1", "", 200},
		{"POST", "/v1.43/containers/mine/exec", `{"Cmd":["sh"]}`, 403},
		{"POST", "/v1.43/containers/mine/update", `{}`, 403},
		{"PUT", "/v1.43/containers/mine/archive?path=/", "x", 403},
		{"POST", "/v1.43/images/create?fromImage=evil%2Fminer", "", 403},
		{"POST", "/v1.43/images/create?fromImage=ft%3A1%40sha256%3Aaa", "", 200},
		{"POST", "/v1.43/networks/create", `{}`, 403},
		{"POST", "/v1.43/build", "", 403},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
	for _, s := range *seen {
		if strings.Contains(s, "theirs") || strings.Contains(s, "exec") || strings.Contains(s, "evil") {
			t.Errorf("refused request reached the socket proxy: %s", s)
		}
	}
}
