# Examples

Demo strategies to try wickfleet with. They show how the pieces fit, not how to
make money: none of them is tuned, and none is advice.

`registry/` is a complete bot registry. `make quickstart` copies it into a local
git repo and starts the stack on it.

| File | What | Source |
|---|---|---|
| `registry/defaults.yaml` | Values every bot inherits: Binance, USDT, 1000 USDT simulated wallet, dry run | wickfleet demo |
| `registry/bots/demo.yaml` | One dry-run bot on EmaCross, BTC ETH SOL | wickfleet demo |
| `registry/strategies/EmaCross.json` | Daily EMA 12/26 cross, only above SMA 200, 15% stop. `.py` is its generated source | wickfleet demo |
| `registry/strategies/RsiReversion.json` | Daily RSI 14 back up through 30 above SMA 200, out at RSI 60, 15% stop. `.py` is its generated source | wickfleet demo |
| `demo/bots/` | Two dry-run bots, EmaCross and RsiReversion, owned by `demo`. A read-only demo registry is `registry/` with these in place of `bots/demo.yaml` (`WICKFLEET_AUTH=demo`, README Login) | wickfleet demo |
| `registry/strategies/SampleStrategy.py` | freqtrade's own sample strategy, 5m, RSI, TEMA and Bollinger bands | [freqtrade](https://github.com/freqtrade/freqtrade) 2026.10, `freqtrade/templates/sample_strategy.py`, GPL-3.0 |
| `equivalence/EmaCrossHand.py` | EmaCross hand-written in plain freqtrade | wickfleet demo |
| `equivalence/RsiReversionHand.py` | RsiReversion hand-written in plain freqtrade | wickfleet demo |

Read a spec in plain words, or turn it into a freqtrade strategy:

```sh
make build
bin/wickfleet-strategy describe examples/registry/strategies/EmaCross.json
bin/wickfleet-strategy gen examples/registry/strategies/EmaCross.json > EmaCross.py
```

`make test` checks that each `.py` next to a spec is exactly what the generator
makes of it.

## Equivalence

`make equivalence` backtests each spec's generated strategy against its
hand-written twin with freqtrade in docker and compares every trade (pair, open
and close time, rates, exit reason, profit). It fails if any trade differs or
if either side makes none. Binance 1d, BTC ETH SOL XRP ADA against USDT,
2023-01-01 to 2026-11-01, freqtrade 2026.10:

| Spec | Trades, generated | Trades, hand-written | Exits |
|---|---|---|---|
| EmaCross | 34 | 34 | 27 signal, 7 stop loss |
| RsiReversion | 7 | 7 | 4 signal, 2 stop loss, 1 still open at the end |

A hand twin with its stop moved from 15% to 12% fails the check at the first
stopped trade, so the comparison does catch a difference.
