# Hand-written twin of the EmaCross demo spec, the way a freqtrade user would write it.
import talib.abstract as ta
import freqtrade.vendor.qtpylib.indicators as qtpylib
from freqtrade.strategy import IStrategy
from pandas import DataFrame


class EmaCrossHand(IStrategy):
    INTERFACE_VERSION = 3
    timeframe = "1d"
    minimal_roi = {"0": 100}
    stoploss = -0.15
    startup_candle_count = 200

    def populate_indicators(self, df: DataFrame, metadata: dict) -> DataFrame:
        df["fast"] = ta.EMA(df, timeperiod=12)
        df["slow"] = ta.EMA(df, timeperiod=26)
        df["trend"] = ta.SMA(df, timeperiod=200)
        return df

    def populate_entry_trend(self, df: DataFrame, metadata: dict) -> DataFrame:
        df.loc[qtpylib.crossed_above(df["fast"], df["slow"]) & (df["close"] > df["trend"]), "enter_long"] = 1
        return df

    def populate_exit_trend(self, df: DataFrame, metadata: dict) -> DataFrame:
        df.loc[qtpylib.crossed_below(df["fast"], df["slow"]), "exit_long"] = 1
        return df
