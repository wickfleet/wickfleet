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
