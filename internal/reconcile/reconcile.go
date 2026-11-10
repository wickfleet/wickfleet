// Package reconcile makes the hub's bot containers match the registry: it
// writes each bot's config and strategy, then creates, replaces or removes
// containers through the Docker API. It only touches containers it labelled.
package reconcile

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wickfleet/internal/botconfig"
	"wickfleet/internal/docker"
	"wickfleet/internal/hostdir"
	"wickfleet/internal/registry"
)

// Engine is the Docker API the reconciler needs.
type Engine interface {
	Managed(ctx context.Context) ([]docker.Container, error)
	Create(ctx context.Context, name string, s docker.Spec) (string, error)
	Start(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	Pull(ctx context.Context, ref string) error
}

// Config is instance-wide. Paths are host paths, mounted at the same place
// inside the hub so it can write what Docker later binds.
type Config struct {
	Images     map[string]string // image key -> pinned reference; bots may only name a key
	Strategies string            // registry checkout's strategies/ dir
	DataDir    string            // bots/<name> = freqtrade user_data
	StateDir   string            // bots/<name>/ config, strategies; bots/<name>.json secrets
	Network    string            // docker network shared with Traefik and the hub
	Host       string            // public host for the Traefik rules
	UIBot      string            // this bot also serves FreqUI at /
	WebhookURL string            // base, bot name appended; empty = off
	BotUser    string            // uid:gid inside the bot, e.g. 1000:1000
	Live       bool              // WICKFLEET_LIVE_TRADING: dry_run: false bots trade real money
	KeysDir    string            // WICKFLEET_EXCHANGE_KEYS_DIR: <bot>.json exchange keys, live only
}

// Reconciler holds no state between runs beyond what is on disk.
type Reconciler struct {
	Cfg    Config
	Docker Engine
	Log    func(format string, args ...any)
}

// Result reports one pass.
type Result struct {
	Created, Replaced, Removed, Unchanged []string
	Errors                                map[string]string
}

const (
	labelBot  = "wickfleet.bot"
	labelHash = "wickfleet.spec-hash"
)

func containerName(bot string) string { return "wickfleet-bot-" + bot }

// Secrets returns the bot's API secrets, making them on first use.
func (r *Reconciler) Secrets(bot string) (botconfig.Secrets, error) {
	if !registry.ValidName(bot) {
		return botconfig.Secrets{}, errors.New("bad bot name")
	}
	path := filepath.Join(r.Cfg.StateDir, "bots", bot+".json")
	var s botconfig.Secrets
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s); err == nil && s.APIPassword != "" && s.JWTSecret != "" {
			return s, nil
		}
	}
	s = botconfig.Secrets{APIPassword: randHex(32), JWTSecret: randHex(32)}
	raw, _ := json.Marshal(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return s, err
	}
	return s, writeAtomic(path, raw, 0o600)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func memBytes(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0
	}
	if strings.HasSuffix(s, "g") {
		return n << 30
	}
	return n << 20
}

// prepare writes the bot's files and returns the container spec.
func (r *Reconciler) prepare(b registry.Bot) (docker.Spec, error) {
	img, ok := r.Cfg.Images[b.Image]
	if !ok {
		return docker.Spec{}, fmt.Errorf("image key %q is not allowed on this instance", b.Image)
	}
	sec, err := r.Secrets(b.Name)
	if err != nil {
		return docker.Spec{}, err
	}
	stateBot := filepath.Join(r.Cfg.StateDir, "bots", b.Name)
	stratDir := filepath.Join(stateBot, "strategies")
	if err := os.MkdirAll(stratDir, 0o750); err != nil {
		return docker.Spec{}, err
	}
	userData := filepath.Join(r.Cfg.DataDir, b.Name)
	if err := hostdir.Make(userData); err != nil {
		return docker.Spec{}, err
	}
	opt := botconfig.Options{WebhookURL: r.Cfg.WebhookURL, Live: r.Cfg.Live}
	if r.Cfg.Live {
		opt.Keys = botconfig.FileKeys(r.Cfg.KeysDir)
	}
	cfg, err := botconfig.Render(b, sec, opt)
	if err != nil {
		return docker.Spec{}, err
	}
	if err := writeAtomic(filepath.Join(stateBot, "config.json"), cfg, 0o640); err != nil {
		return docker.Spec{}, err
	}
	h := sha256.New()
	h.Write([]byte(img))
	h.Write(cfg)
	if b.StrategyFile != "" {
		code, err := os.ReadFile(filepath.Join(r.Cfg.Strategies, b.StrategyFile))
		if err != nil {
			return docker.Spec{}, fmt.Errorf("strategy file: %w", err)
		}
		if err := writeAtomic(filepath.Join(stratDir, b.StrategyFile), code, 0o640); err != nil {
			return docker.Spec{}, err
		}
		h.Write(code)
	}

	cmd := []string{"trade", "--config", "/freqtrade/user_data/config.json", "--strategy", b.Strategy}
	if b.FreqaiModel != "" {
		cmd = append(cmd, "--freqaimodel", b.FreqaiModel)
	}
	labels := r.labels(b)
	spec := docker.Spec{
		Image:  img,
		Cmd:    cmd,
		User:   r.Cfg.BotUser,
		Labels: labels,
		Healthcheck: &docker.Healthcheck{
			Test:        []string{"CMD", "python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/api/v1/ping', timeout=4)"},
			Interval:    int64(30 * time.Second),
			Timeout:     int64(5 * time.Second),
			Retries:     3,
			StartPeriod: int64(120 * time.Second),
		},
		HostConfig: docker.HostConfig{
			Binds: []string{
				userData + ":/freqtrade/user_data",
				filepath.Join(stateBot, "config.json") + ":/freqtrade/user_data/config.json:ro",
				stratDir + ":/freqtrade/user_data/strategies:ro",
			},
			Memory:        memBytes(b.MemLimit),
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			NetworkMode:   r.Cfg.Network,
			CapDrop:       []string{"ALL"},
			SecurityOpt:   []string{"no-new-privileges:true"},
		},
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]docker.Endpoint{
			r.Cfg.Network: {Aliases: []string{"bot-" + b.Name}},
		}},
	}
	for _, k := range sortedKeys(labels) {
		h.Write([]byte(k + "=" + labels[k] + "\n"))
	}
	h.Write([]byte(strings.Join(cmd, " ") + b.MemLimit + r.Cfg.BotUser))
	spec.Labels[labelHash] = hex.EncodeToString(h.Sum(nil))[:16]
	return spec, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// labels are the same Traefik routes the compose stack used.
func (r *Reconciler) labels(b registry.Bot) map[string]string {
	n := b.Name
	rt := "traefik.http.routers.bot-" + n
	l := map[string]string{
		docker.ManagedLabel: "hub",
		labelBot:            n,
		"traefik.enable":    "true",
		rt + ".rule":        fmt.Sprintf("Host(`%s`) && PathPrefix(`/bots/%s/`)", r.Cfg.Host, n),
		rt + ".entrypoints": "web",
		rt + ".priority":    "50",
		rt + ".service":     "bot-" + n,
		rt + ".middlewares": fmt.Sprintf("login@docker,hubkey@docker,hubauth@docker,dropkey@docker,bot-%s-strip", n),
		"traefik.http.middlewares.bot-" + n + "-strip.stripprefix.prefixes": "/bots/" + n,
		"traefik.http.services.bot-" + n + ".loadbalancer.server.port":      "8080",
	}
	if n == r.Cfg.UIBot {
		l["traefik.http.routers.ui.rule"] = fmt.Sprintf("Host(`%s`) && !PathPrefix(`/api`) && !PathPrefix(`/bots/`) && !PathPrefix(`/oauth2/`)", r.Cfg.Host)
		l["traefik.http.routers.ui.entrypoints"] = "web"
		l["traefik.http.routers.ui.priority"] = "1"
		l["traefik.http.routers.ui.service"] = "bot-" + n
		l["traefik.http.routers.ui.middlewares"] = "login@docker,hubkey@docker,hubauth@docker,dropkey@docker"
	}
	return l
}

// Run does one pass. A bot that fails keeps its old container.
func (r *Reconciler) Run(ctx context.Context, bots []registry.Bot) Result {
	res := Result{Errors: map[string]string{}}
	have, err := r.Docker.Managed(ctx)
	if err != nil {
		res.Errors["*"] = err.Error()
		return res
	}
	byBot := map[string]docker.Container{}
	for _, c := range have {
		byBot[c.Labels[labelBot]] = c
	}
	want := map[string]bool{}
	for _, b := range bots {
		want[b.Name] = true
		spec, err := r.prepare(b)
		if err != nil {
			res.Errors[b.Name] = err.Error()
			continue
		}
		cur, exists := byBot[b.Name]
		if exists && cur.Labels[labelHash] == spec.Labels[labelHash] {
			if cur.State != "running" && cur.State != "restarting" {
				if err := r.Docker.Start(ctx, cur.ID); err != nil {
					res.Errors[b.Name] = err.Error()
					continue
				}
			}
			res.Unchanged = append(res.Unchanged, b.Name)
			continue
		}
		if exists {
			if err := r.Docker.Remove(ctx, cur.ID); err != nil {
				res.Errors[b.Name] = err.Error()
				continue
			}
		}
		if err := r.create(ctx, b.Name, spec); err != nil {
			res.Errors[b.Name] = err.Error()
			continue
		}
		if exists {
			res.Replaced = append(res.Replaced, b.Name)
		} else {
			res.Created = append(res.Created, b.Name)
		}
	}
	for name, c := range byBot {
		if want[name] {
			continue
		}
		if err := r.Docker.Remove(ctx, c.ID); err != nil {
			res.Errors[name] = err.Error()
			continue
		}
		res.Removed = append(res.Removed, name)
	}
	sort.Strings(res.Removed)
	// An empty registry is more likely a sync glitch than a real wipe.
	if len(bots) > 0 {
		r.archive(want, res.Errors)
	}
	return res
}

// removedDir holds the dirs of bots that left the registry, under each root.
const removedDir = ".removed"

// archive moves the dirs and secret files of bots no longer wanted to
// .removed/<name>-<UTC stamp>, unless their container failed to go.
func (r *Reconciler) archive(want map[string]bool, errs map[string]string) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for _, root := range []string{r.Cfg.DataDir, filepath.Join(r.Cfg.StateDir, "bots")} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			file := e.Name()
			name, ext := file, ""
			if !e.IsDir() {
				name, ext = strings.TrimSuffix(file, ".json"), ".json"
				if name == file {
					continue
				}
			}
			if strings.HasPrefix(name, ".") || want[name] || errs[name] != "" {
				continue
			}
			dst := filepath.Join(root, removedDir, name+"-"+stamp+ext)
			err := os.MkdirAll(filepath.Dir(dst), 0o750)
			if err == nil {
				err = os.Rename(filepath.Join(root, file), dst)
			}
			if err != nil {
				errs[name] = "archive: " + err.Error()
				continue
			}
			if r.Log != nil {
				r.Log("archived %s to %s", name, dst)
			}
		}
	}
}

func (r *Reconciler) create(ctx context.Context, name string, spec docker.Spec) error {
	id, err := docker.CreatePulling(ctx, r.Docker, containerName(name), spec)
	if err != nil {
		return err
	}
	return r.Docker.Start(ctx, id)
}
