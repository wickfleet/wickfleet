// Package botconfig renders a bot's freqtrade config.json. Dry run is forced
// here as well as in the registry: this file is what freqtrade obeys.
package botconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"wickfleet/internal/registry"
)

// Secrets are the bot's own API credentials, made by the hub.
type Secrets struct {
	APIPassword string `json:"api_password"`
	JWTSecret   string `json:"jwt_secret"`
}

// Options are instance-wide settings.
type Options struct {
	WebhookURL string // base, the bot name is appended; empty turns webhooks off
	Live       bool   // WICKFLEET_LIVE_TRADING: a dry_run: false bot may trade real money
	// Keys returns a live bot's exchange credentials. Only a live instance sets it.
	Keys func(registry.Bot) (Keys, error)
}

// Keys are exchange API credentials. They come from a host file, never from
// the registry, the UI or config_extra.
type Keys struct {
	Key    string `json:"key"`
	Secret string `json:"secret"`
}

// FileKeys reads <dir>/<bot>.json, readable by its owner only. Errors never
// carry the file content.
func FileKeys(dir string) func(registry.Bot) (Keys, error) {
	return func(b registry.Bot) (Keys, error) {
		if dir == "" || !registry.ValidName(b.Name) {
			return Keys{}, fmt.Errorf("bot %s: no exchange key dir", b.Name)
		}
		path := filepath.Join(dir, b.Name+".json")
		st, err := os.Lstat(path)
		if err != nil {
			return Keys{}, fmt.Errorf("bot %s: exchange keys: %w", b.Name, err)
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
			return Keys{}, fmt.Errorf("bot %s: exchange key file must be a regular file, mode 0600 or 0400", b.Name)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return Keys{}, fmt.Errorf("bot %s: exchange keys: %w", b.Name, err)
		}
		var k Keys
		if json.Unmarshal(raw, &k) != nil || k.Key == "" || k.Secret == "" {
			return Keys{}, fmt.Errorf("bot %s: exchange key file needs key and secret", b.Name)
		}
		return k, nil
	}
}

// Render returns the config, indented, stable for hashing.
func Render(b registry.Bot, s Secrets, o Options) ([]byte, error) {
	live := !b.Dry()
	var keys Keys
	if live {
		if !o.Live || o.Keys == nil {
			return nil, fmt.Errorf("bot %s: live trading is not allowed on this instance", b.Name)
		}
		if b.Managed != "" {
			return nil, errors.New("managed bots never trade live")
		}
		k, err := o.Keys(b)
		if err != nil {
			return nil, err
		}
		keys = k
	}
	cfg := map[string]any{
		"bot_name":                   b.Name,
		"max_open_trades":            b.MaxOpenTrades,
		"stake_currency":             b.StakeCurrency,
		"stake_amount":               "unlimited",
		"tradable_balance_ratio":     0.99,
		"dry_run":                    true,
		"dry_run_wallet":             b.Wallet,
		"cancel_open_orders_on_exit": false,
		"trading_mode":               "spot",
		"unfilledtimeout":            map[string]any{"entry": 10, "exit": 10, "exit_timeout_count": 0, "unit": "minutes"},
		"entry_pricing":              map[string]any{"price_side": "same", "use_order_book": true, "order_book_top": 1},
		"exit_pricing":               map[string]any{"price_side": "same", "use_order_book": true, "order_book_top": 1},
		"exchange": map[string]any{
			"name": b.Exchange, "key": "", "secret": "",
			"ccxt_config": map[string]any{}, "ccxt_async_config": map[string]any{},
			"pair_whitelist": b.Pairs, "pair_blacklist": []string{},
		},
		"pairlists": []any{map[string]any{"method": "StaticPairList"}},
		"telegram":  map[string]any{"enabled": false, "token": "", "chat_id": ""},
		"api_server": map[string]any{
			"enabled": true, "listen_ip_address": "0.0.0.0", "listen_port": 8080,
			"verbosity": "error", "enable_openapi": false,
			"jwt_secret_key": s.JWTSecret, "CORS_origins": []string{},
			"username": b.Name, "password": s.APIPassword,
		},
		"initial_state":      "running",
		"force_entry_enable": false,
		"db_url":             "sqlite:////freqtrade/user_data/tradesv3.dryrun.sqlite",
		"internals":          map[string]any{"process_throttle_secs": 5},
	}
	if b.Fee != nil {
		cfg["fee"] = *b.Fee
	}
	if b.Fiat == nil || *b.Fiat != "" {
		fiat := "USD"
		if b.Fiat != nil {
			fiat = *b.Fiat
		}
		cfg["fiat_display_currency"] = fiat
	}
	// Managed bots stay quiet: their controller reports for them.
	if o.WebhookURL != "" && b.Managed == "" {
		cfg["webhook"] = map[string]any{
			"enabled": true, "url": o.WebhookURL + b.Name, "format": "json",
			"retries": 2, "retry_delay": 1,
			"entry_fill": map[string]any{"text": "bought {pair} at {open_rate}, stake {stake_amount} {stake_currency}"},
			"exit_fill":  map[string]any{"text": "sold {pair} at {close_rate}, {profit_amount:.2f} {stake_currency} ({profit_ratio:.2%}), {exit_reason}"},
			"status":     map[string]any{"text": "{status}"},
		}
	}
	merge(cfg, b.ConfigExtra)
	// Whatever config_extra says, these stay: the dry flag, the keys and our API.
	cfg["dry_run"] = !live
	if live {
		cfg["db_url"] = "sqlite:////freqtrade/user_data/tradesv3.sqlite"
	}
	if b.Managed != "" {
		delete(cfg, "webhook")
	}
	cfg["api_server"].(map[string]any)["password"] = s.APIPassword
	cfg["api_server"].(map[string]any)["jwt_secret_key"] = s.JWTSecret
	cfg["api_server"].(map[string]any)["username"] = b.Name
	ex := cfg["exchange"].(map[string]any)
	ex["key"], ex["secret"] = keys.Key, keys.Secret
	return json.MarshalIndent(cfg, "", "  ")
}

// merge copies src into dst, recursing into maps on both sides.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
