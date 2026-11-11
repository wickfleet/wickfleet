package freqtrade

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestTradesPagesAndAddsOpen(t *testing.T) {
	const closed = 1203
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "ma" || p != "pw" {
			http.Error(w, "auth", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/trades":
			off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			n := min(pageSize, closed-off)
			ts := make([]Trade, max(n, 0))
			for i := range ts {
				ts[i] = Trade{FeeOpenCost: f(0.1), FeeCloseCost: f(0.1)}
			}
			json.NewEncoder(w).Encode(map[string]any{"trades": ts, "total_trades": closed})
		case "/api/v1/status":
			json.NewEncoder(w).Encode([]Trade{{FeeOpenCost: f(0.5), IsOpen: true}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := Client{Base: srv.URL, User: "ma", Pass: "pw"}
	ts, err := c.Trades(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != closed+1 {
		t.Fatalf("got %d trades", len(ts))
	}
	var fees float64
	for _, tr := range ts {
		fees += tr.Fees()
	}
	if got := fmt.Sprintf("%.2f", fees); got != "241.10" {
		t.Fatalf("fees %s", got)
	}
}

func TestErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 401)
	}))
	defer srv.Close()
	if _, err := (Client{Base: srv.URL}).Profit(context.Background()); err == nil {
		t.Fatal("want an error on 401")
	}
}

func TestFeesNil(t *testing.T) {
	if (Trade{}).Fees() != 0 {
		t.Fatal("nil fees must be zero")
	}
}

func TestOpenTradesAndDrawdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			// freqtrade 2026.10 field names. Stop distance is a ratio of the current rate.
			w.Write([]byte(`[{"trade_id":7,"pair":"BTC/USDT","base_currency":"BTC","quote_currency":"USDT","is_open":true,
				"amount":0.01,"stake_amount":600.5,"open_rate":60050,"current_rate":61000,"stop_loss_abs":57000,
				"stoploss_current_dist_ratio":-0.06557377,"profit_ratio":0.0158,"open_date":"2026-11-01 10:00:00"}]`))
		case "/api/v1/profit":
			w.Write([]byte(`{"trade_count":3,"max_drawdown":0.125,"max_drawdown_abs":130.2,"current_drawdown":0.05,"current_drawdown_abs":51.1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := Client{Base: srv.URL}
	open, err := c.Status(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("%v %v", open, err)
	}
	o := open[0]
	if o.Pair != "BTC/USDT" || o.Base != "BTC" || o.Quote != "USDT" || o.Amount != 0.01 || o.StakeAmount != 600.5 ||
		o.CurrentRate != 61000 || o.StopLossAbs == nil || *o.StopLossAbs != 57000 || o.StopDistRatio == nil || *o.StopDistRatio > -0.065 {
		t.Fatalf("open trade %+v", o)
	}
	p, err := c.Profit(context.Background())
	if err != nil || p.MaxDrawdown != 0.125 || p.CurrentDrawdown != 0.05 || p.MaxDrawdownAbs != 130.2 || p.CurrentDrawdownAbs != 51.1 {
		t.Fatalf("profit %+v %v", p, err)
	}
}
