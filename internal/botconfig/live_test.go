package botconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wickfleet/internal/registry"
)

const testSecret = "s3cr3t-never-printed"

func liveBot() registry.Bot {
	f := false
	return registry.Bot{Name: "live", Strategy: "S", Exchange: "kraken", Pairs: []string{"BTC/USDT"}, StakeCurrency: "USDT", Wallet: 100, MaxOpenTrades: 1, DryRun: &f}
}

func writeKeys(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(dir, name+".json"), mode)
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLiveBotOnADryInstanceIsRefused(t *testing.T) {
	_, err := Render(liveBot(), Secrets{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("got %v", err)
	}
	// Live on but no key source: still refused.
	if _, err := Render(liveBot(), Secrets{}, Options{Live: true}); err == nil {
		t.Fatal("live without a key source rendered")
	}
}

func TestLiveRenderTakesKeysFromTheFileOnly(t *testing.T) {
	dir := t.TempDir()
	writeKeys(t, dir, "live", `{"key":"k1","secret":"`+testSecret+`"}`, 0o600)
	b := liveBot()
	b.ConfigExtra = map[string]any{"dry_run": true, "exchange": map[string]any{"key": "evil", "secret": "evil"}}
	raw, err := Render(b, Secrets{}, Options{Live: true, Keys: FileKeys(dir)})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, raw)
	ex := m["exchange"].(map[string]any)
	if m["dry_run"] != false || ex["key"] != "k1" || ex["secret"] != testSecret {
		t.Fatalf("live config: dry_run=%v key=%v", m["dry_run"], ex["key"])
	}
	if !strings.Contains(m["db_url"].(string), "tradesv3.sqlite") || strings.Contains(m["db_url"].(string), "dryrun") {
		t.Fatalf("live db_url %v", m["db_url"])
	}
}

func TestDryBotOnALiveInstanceGetsNoKeys(t *testing.T) {
	called := false
	keys := func(registry.Bot) (Keys, error) { called = true; return Keys{Key: "k", Secret: testSecret}, nil }
	b := liveBot()
	b.DryRun = nil
	b.ConfigExtra = map[string]any{"dry_run": false}
	raw, err := Render(b, Secrets{}, Options{Live: true, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, raw)
	ex := m["exchange"].(map[string]any)
	if called || m["dry_run"] != true || ex["key"] != "" || ex["secret"] != "" {
		t.Fatalf("dry bot: called=%v dry_run=%v key=%v", called, m["dry_run"], ex["key"])
	}
}

func TestFileKeysRefusesAndNeverLeaks(t *testing.T) {
	dir := t.TempDir()
	b := liveBot()
	get := FileKeys(dir)
	cases := map[string]func(){
		"missing":        func() {},
		"group readable": func() { writeKeys(t, dir, "live", `{"key":"k","secret":"`+testSecret+`"}`, 0o640) },
		"no secret":      func() { writeKeys(t, dir, "live", `{"key":"`+testSecret+`"}`, 0o600) },
		"not json":       func() { writeKeys(t, dir, "live", testSecret, 0o600) },
		"symlink": func() {
			os.Remove(filepath.Join(dir, "live.json"))
			writeKeys(t, dir, "real", `{"key":"k","secret":"`+testSecret+`"}`, 0o600)
			os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(dir, "live.json"))
		},
	}
	for _, name := range []string{"missing", "group readable", "no secret", "not json", "symlink"} {
		os.Remove(filepath.Join(dir, "live.json"))
		cases[name]()
		_, err := get(b)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), testSecret) {
			t.Fatalf("%s: error leaks the secret: %v", name, err)
		}
	}
	if _, err := FileKeys("")(b); err == nil {
		t.Fatal("empty dir accepted")
	}
}
