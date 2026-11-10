package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ma.yaml", "name: ma\nowner: alice\nstrategy: DryRun\npairs: [BTC/USDT]\n")
	write(t, dir, "sample.yaml", "name: sample\nowner: bob\nstrategy: SampleStrategy\ndry_run: true\n")
	write(t, dir, "notes.txt", "ignored")
	bots, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bots) != 2 || bots[0].Name != "ma" || bots[1].Name != "sample" {
		t.Fatalf("got %+v", bots)
	}
	if !bots[0].Dry() || !bots[1].Dry() {
		t.Fatalf("dry flags wrong: %v %v", bots[0].Dry(), bots[1].Dry())
	}
	if b, ok := Find(bots, "sample"); !ok || b.Owner != "bob" {
		t.Fatalf("find: %+v %v", b, ok)
	}
	if _, ok := Find(bots, "nope"); ok {
		t.Fatal("found a bot that does not exist")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]struct{ file, body, want string }{
		"bad name":      {"Ma.yaml", "name: Ma\nowner: x\nstrategy: S\n", "bad bot name"},
		"no owner":      {"ma.yaml", "name: ma\nstrategy: S\n", "owner missing"},
		"no strategy":   {"ma.yaml", "name: ma\nowner: x\n", "strategy missing"},
		"file mismatch": {"other.yaml", "name: ma\nowner: x\nstrategy: S\n", "does not match"},
		"unknown field": {"ma.yaml", "name: ma\nowner: x\nstrategy: S\napi_key: k\n", "api_key"},
		"path in name":  {"ma.yaml", "name: ../ma\nowner: x\nstrategy: S\n", "bad bot name"},
		"live":          {"ma.yaml", "name: ma\nowner: x\nstrategy: S\ndry_run: false\n", "live trading"},
		"file path":     {"ma.yaml", "name: ma\nowner: x\nstrategy: S\nstrategy_file: ../../etc/x.py\n", "bad strategy_file"},
		"class inject":  {"ma.yaml", "name: ma\nowner: x\nstrategy: \"S --userdir /\"\n", "bad strategy class"},
		"mem":           {"ma.yaml", "name: ma\nowner: x\nstrategy: S\nmem_limit: 99t\n", "bad mem_limit"},
		"watch op":      {"ma.yaml", "name: ma\nowner: x\nstrategy: S\nwatch: [{left: close, op: \"=>\", right: hi20}]\n", "bad op"},
		"managed tag":   {"ma.yaml", "name: ma\nowner: x\nstrategy: S\narena: \"AI zoo\"\n", "bad managed tag"},
		"watch field":   {"ma.yaml", "name: ma\nowner: x\nstrategy: S\nwatch: [{left: close, op: \">\", rigth: hi20}]\n", "rigth"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, c.file, c.body)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"ma", "a1", "bot-2"} {
		if !ValidName(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "1a", "a_b", "a/b", "A", strings.Repeat("a", 32)} {
		if ValidName(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestLoadRepo(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bots"), 0o755)
	write(t, root, "defaults.yaml", "exchange: kraken\nfee: 0.008\nfiat: USD\nwallet: 1000\nimage: freqtrade\n")
	write(t, filepath.Join(root, "bots"), "ndax.yaml", "name: ndax\nowner: alice\nstrategy: EmaCross\nstrategy_file: EmaCross.py\nexchange: ndax\nfee: 0.002\nfiat: \"\"\nconfig_extra: {timeframe: 1h}\n")
	write(t, filepath.Join(root, "bots"), "plain.yaml", "name: plain\nowner: bob\nstrategy: S\n")
	bots, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	n, p := bots[0], bots[1]
	if n.Exchange != "ndax" || *n.Fee != 0.002 || *n.Fiat != "" || n.Wallet != 1000 || n.ConfigExtra["timeframe"] != "1h" {
		t.Fatalf("bot override wrong: %+v", n)
	}
	if p.Exchange != "kraken" || *p.Fee != 0.008 || *p.Fiat != "USD" || p.Image != "freqtrade" {
		t.Fatalf("defaults not applied: %+v", p)
	}
	write(t, root, "defaults.yaml", "exchange: kraken\nbogus: 1\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown default key must fail, got %v", err)
	}
}

func TestLoadMissingDirFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "not-there")); err == nil {
		t.Fatal("a missing registry must be an error, not zero bots")
	}
}

func TestLoadWatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "trend.yaml", "name: trend\nowner: x\nstrategy: EmaCross\nwatch:\n  - {name: breakout, left: close, op: \">\", right: hi20}\n  - {left: close, op: \">\", right: sma200}\n")
	bots, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := bots[0].Watch
	if len(w) != 2 || w[0].Label() != "breakout" || w[1].Label() != "close > sma200" {
		t.Fatalf("watch %+v", w)
	}
}

func TestLoadManaged(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "founder.yaml", "name: founder\nowner: x\nstrategy: S\narena: founders\n")
	write(t, dir, "plain.yaml", "name: plain\nowner: x\nstrategy: S\n")
	bots, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bots[0].Managed != "founders" || bots[1].Managed != "" {
		t.Fatalf("managed tag %q %q", bots[0].Managed, bots[1].Managed)
	}
}

func TestParseBotAppliesDefaultsAndRules(t *testing.T) {
	b, err := ParseBot([]byte("name: kid\nowner: x\nstrategy: S\narena: ai\n"), map[string]any{"exchange": "binance", "wallet": 1000})
	if err != nil || b.Exchange != "binance" || b.Wallet != 1000 || b.Managed != "ai" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := ParseBot([]byte("name: kid\nowner: x\nstrategy: S\ntypo: 1\n"), nil); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := ParseBot([]byte("name: Kid\nowner: x\nstrategy: S\n"), nil); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestLivePolicy(t *testing.T) {
	raw := []byte("name: live\nowner: alice\nstrategy: S\ndry_run: false\n")
	if _, err := ParseBot(raw, nil); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("default policy: %v", err)
	}
	b, err := ParseBotWith(raw, nil, Policy{AllowLive: true})
	if err != nil || b.Dry() {
		t.Fatalf("allow live: %v dry=%v", err, b.Dry())
	}
	managed := []byte("name: agent\nowner: hub\nstrategy: S\narena: zoo\ndry_run: false\n")
	if _, err := ParseBotWith(managed, nil, Policy{AllowLive: true}); err == nil {
		t.Fatal("managed bot may trade live")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "live.yaml"), raw, 0o644)
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a live bot")
	}
	if bots, err := LoadWith(dir, Policy{AllowLive: true}); err != nil || len(bots) != 1 {
		t.Fatalf("LoadWith: %v", err)
	}
}
