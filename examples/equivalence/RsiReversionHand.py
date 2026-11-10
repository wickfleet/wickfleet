# Hand-written twin of the RsiReversion demo spec, the way a freqtrade user would write it.
import talib.abstract as ta
import freqtrade.vendor.qtpylib.indicators as qtpylib
from freqtrade.strategy import IStrategy
from pandas import DataFrame


class RsiReversionHand(IStrategy):
    INTERFACE_VERSION = 3
    timeframe = "1d"
    minimal_roi = {"0": 100}
    stoploss = -0.15
    startup_candle_count = 200

    def populate_indicators(self, df: DataFrame, metadata: dict) -> DataFrame:
        df["rsi"] = ta.RSI(df, timeperiod=14)
        df["trend"] = ta.SMA(df, timeperiod=200)
        return df

    def populate_entry_trend(self, df: DataFrame, metadata: dict) -> DataFrame:
        df.loc[qtpylib.crossed_above(df["rsi"], 30) & (df["close"] > df["trend"]), "enter_long"] = 1
        return df

    def populate_exit_trend(self, df: DataFrame, metadata: dict) -> DataFrame:
        df.loc[df["rsi"] >= 60, "exit_long"] = 1
        return df
