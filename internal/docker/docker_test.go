package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient(t *testing.T) {
	var created Spec
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1.43/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), "wickfleet.managed=hub") || r.URL.Query().Get("all") != "1" {
				t.Errorf("list filter %q", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode([]Container{{ID: "c1", Names: []string{"/wickfleet-bot-ma"}, Labels: map[string]string{"wickfleet.bot": "ma"}, State: "running"}})
		case r.Method == "POST" && r.URL.Path == "/v1.43/containers/create":
			if r.URL.Query().Get("name") == "missing-image" {
				http.Error(w, "No such image", 404)
				return
			}
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(201)
			w.Write([]byte(`{"Id":"new1"}`))
		case r.Method == "POST" && r.URL.Path == "/v1.43/containers/new1/start":
			w.WriteHeader(204)
		case r.Method == "DELETE" && r.URL.Path == "/v1.43/containers/c1":
			if r.URL.Query().Get("force") != "1" {
				t.Error("remove must force")
			}
			w.WriteHeader(204)
		case r.Method == "POST" && r.URL.Path == "/v1.43/images/create":
			if r.URL.Query().Get("fromImage") == "bad" {
				w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"error":"manifest unknown"}`))
				return
			}
			if r.URL.Query().Get("fromImage") != "img:1@sha256:abc" {
				t.Errorf("pull ref %q", r.URL.Query().Get("fromImage"))
			}
			w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Done"}`))
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	c := Client{Base: srv.URL}
	ctx := context.Background()
	got, err := c.Managed(ctx)
	if err != nil || len(got) != 1 || got[0].Labels["wickfleet.bot"] != "ma" {
		t.Fatalf("%v %+v", err, got)
	}
	id, err := c.Create(ctx, "wickfleet-bot-ma", Spec{Image: "img", Labels: map[string]string{"a": "b"}, HostConfig: HostConfig{Binds: []string{"/x:/y"}}})
	if err != nil || id != "new1" || created.Image != "img" || created.HostConfig.Binds[0] != "/x:/y" || created.HostConfig.Privileged {
		t.Fatalf("%v %s %+v", err, id, created)
	}
	if err := c.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Pull(ctx, "img:1@sha256:abc"); err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(ctx, "missing-image", Spec{})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("want a 404 StatusError, got %v", err)
	}
	if err := c.Pull(ctx, "bad"); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("in-stream pull error must surface, got %v", err)
	}
	if len(calls) != 7 {
		t.Fatalf("calls %v", calls)
	}
}

func TestInspectAndLogs(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		n := len(s)
		return append([]byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, s...)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.43/containers/j1/json":
			w.Write([]byte(`{"State":{"Status":"exited","Running":false,"ExitCode":2,"OOMKilled":true}}`))
		case "/v1.43/containers/j1/logs":
			if r.URL.Query().Get("tail") != "50" || r.URL.Query().Get("stderr") != "1" {
				t.Errorf("logs query %q", r.URL.RawQuery)
			}
			w.Write(append(frame(1, "downloading\n"), frame(2, "ERROR - no data\n")...))
		case "/v1.43/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), "wickfleet.managed=replay") {
				t.Errorf("filter %q", r.URL.RawQuery)
			}
			w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	c := Client{Base: srv.URL}
	ctx := context.Background()
	st, err := c.Inspect(ctx, "j1")
	if err != nil || st.Status != "exited" || st.ExitCode != 2 || !st.OOMKill {
		t.Fatalf("%v %+v", err, st)
	}
	logs, err := c.Logs(ctx, "j1", 50)
	if err != nil || logs != "downloading\nERROR - no data\n" {
		t.Fatalf("%v %q", err, logs)
	}
	if _, err := c.Labeled(ctx, "replay"); err != nil {
		t.Fatal(err)
	}
}
