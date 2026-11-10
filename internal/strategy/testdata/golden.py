"""Synthetic candles through freqtrade's data cleaning and the generated AllKinds
strategy's indicators and signals, as JSON; run by `make strategy-golden`."""
import json
import math
import random
import sys

import pandas as pd
from freqtrade.data.converter import clean_ohlcv_dataframe

sys.path.insert(0, "/w")
from AllKinds import AllKinds  # noqa: E402

HOUR = 3600


def candles(seed, price, n, gap=(0, 0), flat=(0, 0)):
    rnd = random.Random(seed)
    r = (lambda x: round(x, 2)) if price > 1 else (lambda x: float(f"{x:.6g}"))
    rows, t = [], 1_700_000_000 // HOUR * HOUR
    for i in range(n):
        if gap[0] <= i < gap[1]:
            t += HOUR
            continue
        o = price
        c = o if flat[0] <= i < flat[1] else o * math.exp(rnd.gauss(0, 0.01))
        h = o if flat[0] <= i < flat[1] else max(o, c) * (1 + abs(rnd.gauss(0, 0.004)))
        lo = o if flat[0] <= i < flat[1] else min(o, c) * (1 - abs(rnd.gauss(0, 0.004)))
        o, h, lo, c = r(o), r(h), r(lo), r(c)
        rows.append([t * 1000, o, h, lo, c, round(rnd.uniform(10, 1000), 3)])
        price, t = c, t + HOUR
    return rows


def run(rows):
    df = pd.DataFrame(rows, columns=["date", "open", "high", "low", "close", "volume"])
    df["date"] = pd.to_datetime(df["date"], unit="ms", utc=True)
    df = clean_ohlcv_dataframe(df, "1h", pair="X/USDT", fill_missing=True, drop_incomplete=False)
    s = AllKinds({})
    meta = {"pair": "X/USDT"}
    df = s.populate_exit_trend(s.populate_entry_trend(s.populate_indicators(df, meta), meta), meta)
    out = {"time": [int(d.timestamp()) for d in df["date"]]}
    for c in df.columns:
        if c != "date":
            out[c] = [None if pd.isna(v) else float(v) for v in df[c]]
    return {"candles": rows, "columns": out}


json.dump({
    "btc": run(candles(1, 60000.0, 300, gap=(100, 105), flat=(220, 232))),
    "tiny": run(candles(2, 0.00002, 150)),
    "dust": run(candles(3, 5e-7, 150)),
}, sys.stdout)
