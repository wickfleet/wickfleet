// Package docker is the slice of the Docker Engine API the reconciler uses,
// spoken over HTTP to a socket proxy.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// APIVersion is supported by Docker 25 and later.
const APIVersion = "v1.43"

// ManagedLabel marks containers the hub owns; it never touches others.
const ManagedLabel = "wickfleet.managed"

// Client talks to e.g. http://socket-proxy-hub:2375.
type Client struct {
	Base string
	HTTP *http.Client
}

// Container is the list view.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

// Spec is the create body. Only the fields the hub sets.
type Spec struct {
	Image            string              `json:"Image"`
	Entrypoint       []string            `json:"Entrypoint,omitempty"`
	Cmd              []string            `json:"Cmd"`
	User             string              `json:"User,omitempty"`
	Labels           map[string]string   `json:"Labels"`
	Healthcheck      *Healthcheck        `json:"Healthcheck,omitempty"`
	HostConfig       HostConfig          `json:"HostConfig"`
	NetworkingConfig *NetworkingConfig   `json:"NetworkingConfig,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
}

type Healthcheck struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	Retries     int      `json:"Retries"`
	StartPeriod int64    `json:"StartPeriod"`
}

type HostConfig struct {
	Binds         []string      `json:"Binds"`
	Memory        int64         `json:"Memory,omitempty"`
	CPUShares     int64         `json:"CpuShares,omitempty"` // relative weight; Docker's default is 1024
	RestartPolicy RestartPolicy `json:"RestartPolicy"`
	NetworkMode   string        `json:"NetworkMode"`
	Privileged    bool          `json:"Privileged"`
	ReadonlyRoot  bool          `json:"ReadonlyRootfs"`
	CapDrop       []string      `json:"CapDrop,omitempty"`
	SecurityOpt   []string      `json:"SecurityOpt,omitempty"`
}

type RestartPolicy struct {
	Name string `json:"Name"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]Endpoint `json:"EndpointsConfig"`
}

type Endpoint struct {
	Aliases []string `json:"Aliases"`
}

// StatusError carries the Engine's HTTP status, so callers can tell a
// missing image (404 on create) from anything else.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("docker: HTTP %d: %s", e.Code, e.Body) }

func (c Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	u := c.Base + "/" + APIVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute} // image pulls stream for a while
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	if st, ok := out.(interface{ UnmarshalStream(io.Reader) error }); ok {
		return st.UnmarshalStream(resp.Body)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// Managed lists the hub's bot containers, running or not.
func (c Client) Managed(ctx context.Context) ([]Container, error) {
	return c.Labeled(ctx, "hub")
}

// Labeled lists containers whose ManagedLabel has this value, e.g. "replay".
func (c Client) Labeled(ctx context.Context, value string) ([]Container, error) {
	f, _ := json.Marshal(map[string][]string{"label": {ManagedLabel + "=" + value}})
	var out []Container
	return out, c.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out)
}

// State is the part of an inspect the job runner reads.
type State struct {
	Status   string `json:"Status"` // created, running, exited, dead, ...
	Running  bool   `json:"Running"`
	ExitCode int    `json:"ExitCode"`
	OOMKill  bool   `json:"OOMKilled"`
}

// Inspect returns the container's state.
func (c Client) Inspect(ctx context.Context, id string) (State, error) {
	var out struct {
		State State `json:"State"`
	}
	return out.State, c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
}

// Logs returns the last lines of stdout and stderr, merged.
func (c Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	var out logStream
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprint(tail)}}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil, &out)
	return out.String(), err
}

// logStream decodes the multiplexed log format of a container without a TTY:
// 8-byte frame headers (stream, 0, 0, 0, size BE uint32) before each chunk.
type logStream struct{ b bytes.Buffer }

func (l *logStream) String() string { return l.b.String() }

func (l *logStream) UnmarshalStream(r io.Reader) error {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		n := int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])
		if _, err := io.CopyN(&l.b, r, n); err != nil {
			return err
		}
	}
}

// Create makes a container and returns its id.
func (c Client) Create(ctx context.Context, name string, s Spec) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	return out.ID, c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, s, &out)
}

func (c Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// Remove stops and deletes; anonymous volumes go too, bind mounts stay.
func (c Client) Remove(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
}

// Pull fetches an image; ref may carry a tag and a digest. The Engine
// answers 200 and reports failures inside the progress stream.
func (c Client) Pull(ctx context.Context, ref string) error {
	var stream pullStream
	if err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, &stream); err != nil {
		return err
	}
	if stream.err != "" {
		return fmt.Errorf("pull %s: %s", ref, stream.err)
	}
	return nil
}

// pullStream reads newline-separated progress objects and keeps the first error.
type pullStream struct{ err string }

func (p *pullStream) UnmarshalStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var m struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" && p.err == "" {
			p.err = m.Error
		}
	}
}

// Creator is the part of an engine CreatePulling needs.
type Creator interface {
	Create(ctx context.Context, name string, s Spec) (string, error)
	Pull(ctx context.Context, ref string) error
}

// CreatePulling creates a container, pulling its image once when Docker does
// not have it (a digest-only image is dangling and the weekly prune drops it).
func CreatePulling(ctx context.Context, e Creator, name string, s Spec) (string, error) {
	id, err := e.Create(ctx, name, s)
	var se *StatusError
	if errors.As(err, &se) && se.Code == 404 {
		if perr := e.Pull(ctx, s.Image); perr != nil {
			return "", perr
		}
		id, err = e.Create(ctx, name, s)
	}
	return id, err
}
