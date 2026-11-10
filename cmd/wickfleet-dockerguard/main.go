// wickfleet-dockerguard sits between the hub and the Docker socket proxy and
// refuses any Docker request the hub would never make itself.
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"wickfleet/internal/docker"
	"wickfleet/internal/guard"
)

func list(name string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(name), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	up, err := url.Parse(os.Getenv("WFGUARD_UPSTREAM"))
	if err != nil || up.Host == "" {
		log.Fatal("WFGUARD_UPSTREAM must be the socket proxy URL, e.g. http://socket-proxy-hub:2375")
	}
	p := guard.Policy{
		Images:       list("WFGUARD_IMAGES"),
		BindPrefixes: list("WFGUARD_BIND_PREFIXES"),
		Networks:     list("WFGUARD_NETWORKS"),
		Users:        list("WFGUARD_USERS"),
		NamePrefix:   "wickfleet-",
		ManagedLabel: docker.ManagedLabel,
	}
	if len(p.Images) == 0 || len(p.BindPrefixes) == 0 || len(p.Networks) == 0 || len(p.Users) == 0 {
		log.Fatal("WFGUARD_IMAGES, WFGUARD_BIND_PREFIXES, WFGUARD_NETWORKS and WFGUARD_USERS must all be set")
	}
	addr := os.Getenv("WFGUARD_LISTEN")
	if addr == "" {
		addr = ":2375"
	}
	g := &guard.Guard{Policy: p, Upstream: up, Client: &http.Client{Timeout: 10 * time.Second}, Log: log.Printf}
	srv := &http.Server{Addr: addr, Handler: g, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("wickfleet-dockerguard on %s -> %s, %d images, %d bind prefixes", addr, up, len(p.Images), len(p.BindPrefixes))
	log.Fatal(srv.ListenAndServe())
}
