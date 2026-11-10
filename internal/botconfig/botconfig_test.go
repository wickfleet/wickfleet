package botconfig

import (
	"encoding/json"
	"testing"

	"wickfleet/internal/registry"
)

func ptr[T any](v T) *T { return &v }

func render(t *testing.T, b registry.Bot, o Options) map[string]any {
	t.Helper()
	raw, err := Render(b, Secrets{APIPassword: "pw", JWTSecret: "jwt"}, o)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRender(t *testing.T) {
	b := registry.Bot{Name: "ma", Strategy: "S", Exchange: "kraken", Pairs: []string{"BTC/USDT"}, StakeCurrency: "USDT",
		Wallet: 1000, MaxOpenTrades: 3, Fee: ptr(0.008)}
	m := render(t, b, Options{WebhookURL: "http://hub:8090/hub/webhook/"})
	if m["fee"] != 0.008 || m["fiat_display_currency"] != "USD" || m["dry_run"] != true {
		t.Fatalf("%v %v %v", m["fee"], m["fiat_display_currency"], m["dry_run"])
	}
	api := m["api_server"].(map[string]any)
	if api["username"] != "ma" || api["password"] != "pw" || api["jwt_secret_key"] != "jwt" {
		t.Fatalf("api %v", api)
	}
	if m["webhook"].(map[string]any)["url"] != "http://hub:8090/hub/webhook/ma" {
		t.Fatalf("webhook %v", m["webhook"])
	}
	m = render(t, registry.Bot{Name: "ma", Fiat: ptr("")}, Options{})
	if _, ok := m["fiat_display_currency"]; ok {
		t.Fatal("empty fiat must leave the key out")
	}
	if _, ok := m["webhook"]; ok {
		t.Fatal("no webhook url must leave webhooks off")
	}
	if _, ok := m["fee"]; ok {
		t.Fatal("unset fee must leave the key out")
	}
}

func TestConfigExtraCannotGoLive(t *testing.T) {
	b := registry.Bot{Name: "ma", ConfigExtra: map[string]any{
		"dry_run":    false,
		"timeframe":  "1h",
		"exchange":   map[string]any{"key": "stolen", "secret": "x", "ccxt_config": map[string]any{"rateLimit": 500}},
		"api_server": map[string]any{"password": "attacker", "listen_port": 8080},
		"freqai":     map[string]any{"enabled": true},
	}}
	m := render(t, b, Options{})
	ex := m["exchange"].(map[string]any)
	api := m["api_server"].(map[string]any)
	if m["dry_run"] != true || ex["key"] != "" || ex["secret"] != "" || api["password"] != "pw" {
		t.Fatalf("config_extra overrode a guarded key: dry=%v ex=%v api=%v", m["dry_run"], ex, api)
	}
	if m["timeframe"] != "1h" || ex["ccxt_config"].(map[string]any)["rateLimit"] != float64(500) || m["freqai"] == nil {
		t.Fatalf("config_extra merge lost data: %v %v", m["timeframe"], ex)
	}
}

func TestStable(t *testing.T) {
	b := registry.Bot{Name: "ma", ConfigExtra: map[string]any{"a": 1, "b": map[string]any{"c": 2}}}
	x, _ := Render(b, Secrets{}, Options{})
	y, _ := Render(b, Secrets{}, Options{})
	if string(x) != string(y) {
		t.Fatal("render must be deterministic for hashing")
	}
}

func TestManagedBotHasNoWebhook(t *testing.T) {
	o := Options{WebhookURL: "http://hub:8090/hub/webhook/"}
	b := registry.Bot{Name: "a1", Managed: "zoo", ConfigExtra: map[string]any{"webhook": map[string]any{"enabled": true, "url": "http://hub:8090/hub/webhook/a1"}}}
	m := render(t, b, o)
	if _, ok := m["webhook"]; ok {
		t.Fatalf("managed bot got a webhook: %v", m["webhook"])
	}
	if m["telegram"].(map[string]any)["enabled"] != false {
		t.Fatalf("telegram %v", m["telegram"])
	}
	m = render(t, registry.Bot{Name: "ma"}, o)
	if m["webhook"].(map[string]any)["url"] != "http://hub:8090/hub/webhook/ma" {
		t.Fatalf("plain bot webhook %v", m["webhook"])
	}
}
