package freqtrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTimeframeAndLastCandle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "trend" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/show_config":
			_, _ = w.Write([]byte(`{"timeframe":"1d"}`))
		case "/api/v1/pair_candles":
			if r.URL.Query().Get("pair") != "BTC/USDT" || r.URL.Query().Get("timeframe") != "1d" || r.URL.Query().Get("limit") != "1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"columns":["date","close","hi20","enter_long"],"data":[["2026-10-19",82000,87000,null],["2026-10-20",83024.7,87230.2,null]]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := Client{Base: srv.URL, User: "trend", Pass: "pw"}
	tf, err := c.Timeframe(context.Background())
	if err != nil || tf != "1d" {
		t.Fatalf("timeframe %q %v", tf, err)
	}
	row, err := c.LastCandle(context.Background(), "BTC/USDT", tf)
	if err != nil {
		t.Fatal(err)
	}
	if row["date"] != "2026-10-20" || row["close"] != 83024.7 || row["hi20"] != 87230.2 || row["enter_long"] != nil {
		t.Fatalf("row %+v", row)
	}
	if _, err := (Client{Base: srv.URL, User: "trend", Pass: "bad"}).Timeframe(context.Background()); err == nil {
		t.Fatal("bad password passed")
	}
}
