package strategy

import (
	"math"
	"strconv"
)

// Frame is candles as columns, oldest first, as freqtrade hands them to
// populate_indicators. Time is each candle's open in Unix seconds.
type Frame struct {
	Time                           []int64
	Open, High, Low, Close, Volume []float64
}

func (f Frame) price(name string) []float64 {
	switch name {
	case "open":
		return f.Open
	case "high":
		return f.High
	case "low":
		return f.Low
	case "close":
		return f.Close
	case "volume":
		return f.Volume
	}
	return nil
}

// TimeframeSeconds is a candle's length, e.g. 3600 for "1h"; 0 if unknown.
func TimeframeSeconds(tf string) int64 {
	if len(tf) < 2 {
		return 0
	}
	n, err := strconv.ParseInt(tf[:len(tf)-1], 10, 64)
	if err != nil || n < 1 {
		return 0
	}
	unit := map[byte]int64{'m': 60, 'h': 3600, 'd': 86400, 'w': 604800}[tf[len(tf)-1]]
	return n * unit
}

// Columns computes the generated strategy's columns on f with the arithmetic of
// its pandas and TA-Lib 0.8 calls; warmup rows are NaN, model columns absent.
func (s Spec) Columns(f Frame) map[string][]float64 {
	out := map[string][]float64{}
	for _, in := range s.Indicators {
		src, id := f.price(in.Source), in.ID
		switch in.Kind {
		case "sma":
			out[id] = shift(rollingMean(src, in.Period), in.Shift)
		case "highest":
			out[id] = shift(rollingExtreme(src, in.Period, func(a, b float64) bool { return a > b }), in.Shift)
		case "lowest":
			out[id] = shift(rollingExtreme(src, in.Period, func(a, b float64) bool { return a < b }), in.Shift)
		case "ema":
			out[id] = shift(ema(src, in.Period), in.Shift)
		case "tema":
			out[id] = shift(tema(src, in.Period), in.Shift)
		case "rsi":
			out[id] = rsi(src, in.Period)
		case "atr":
			out[id] = atr(f.High, f.Low, f.Close, in.Period)
		case "adx":
			out[id] = adx(f.High, f.Low, f.Close, in.Period)
		case "bollinger":
			out[id+".upper"], out[id+".middle"], out[id+".lower"] = bbands(src, in.Period, in.StdDev)
		case "macd":
			out[id+".macd"], out[id+".signal"], out[id+".hist"] = macd(src, in.Fast, in.Slow, in.Signal)
		case "stoch":
			out[id+".k"], out[id+".d"] = stoch(f.High, f.Low, f.Close, in.Period, in.SmoothK, in.SmoothD)
		}
	}
	return out
}

func nans(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

func shift(x []float64, n int) []float64 {
	if n <= 0 {
		return x
	}
	out := nans(len(x))
	if n < len(x) {
		copy(out[n:], x[:len(x)-n])
	}
	return out
}

// rollingMean is pandas' rolling(p).mean(): Kahan sums into and out of the window.
func rollingMean(x []float64, p int) []float64 {
	out := nans(len(x))
	var sum, cAdd, cRem, prev float64
	nobs, neg, same := 0, 0, 0
	add := func(v float64) {
		if v != v {
			return
		}
		nobs++
		y := v - cAdd
		t := sum + y
		cAdd = t - sum - y
		sum = t
		if math.Signbit(v) {
			neg++
		}
		if v == prev {
			same++
		} else {
			same = 1
		}
		prev = v
	}
	remove := func(v float64) {
		if v != v {
			return
		}
		nobs--
		y := -v - cRem
		t := sum + y
		cRem = t - sum - y
		sum = t
		if math.Signbit(v) {
			neg--
		}
	}
	for i := range x {
		s := max(0, i-p+1)
		if i == 0 || s >= i {
			sum, cAdd, cRem, nobs, neg, same, prev = 0, 0, 0, 0, 0, 0, x[s]
			for j := s; j <= i; j++ {
				add(x[j])
			}
		} else {
			for j := max(0, i-p); j < s; j++ {
				remove(x[j])
			}
			add(x[i])
		}
		if nobs >= p && nobs > 0 {
			r := sum / float64(nobs)
			switch {
			case same >= nobs:
				r = prev
			case neg == 0 && r < 0, neg == nobs && r > 0:
				r = 0
			}
			out[i] = r
		}
	}
	return out
}

// rollingExtreme is pandas' rolling(p).max() or .min(), by better.
func rollingExtreme(x []float64, p int, better func(a, b float64) bool) []float64 {
	out := nans(len(x))
	for i := p - 1; i >= 0 && i < len(x); i++ {
		m := x[i-p+1]
		for _, v := range x[i-p+2 : i+1] {
			if better(v, m) {
				m = v
			}
		}
		out[i] = m
	}
	return out
}

// sma is TA_SMA over all of x: len(x)-p+1 values, the first for x[p-1].
func sma(x []float64, p int) []float64 {
	if p < 1 || len(x) < p {
		return nil
	}
	if p == 1 {
		return append([]float64{}, x...)
	}
	out := make([]float64, 0, len(x)-p+1)
	var total float64
	for _, v := range x[:p-1] {
		total += v
	}
	for i := p - 1; i < len(x); i++ {
		total += x[i]
		t := total
		total -= x[i-p+1]
		out = append(out, t/float64(p))
	}
	return out
}

// ema is TA_EMA: seeded with the average of the first p values.
func ema(x []float64, p int) []float64 {
	out := nans(len(x))
	if p < 1 || len(x) < p {
		return out
	}
	if p == 1 {
		copy(out, x)
		return out
	}
	k := 2.0 / float64(p+1)
	var t float64
	for _, v := range x[:p] {
		t += v
	}
	prev := t / float64(p)
	out[p-1] = prev
	for i := p; i < len(x); i++ {
		prev = math.FMA(x[i]-prev, k, prev)
		out[i] = prev
	}
	return out
}

// tema is TA_TEMA: three EMAs in lockstep, each seeded by the average of the
// first p values of the one before.
func tema(x []float64, p int) []float64 {
	n, look := len(x), 3*(p-1)
	out := nans(n)
	if p < 1 || n <= look {
		return out
	}
	if p == 1 {
		copy(out, x)
		return out
	}
	k, fp, today := 2.0/float64(p+1), float64(p), 0
	var t float64
	for ; today < p; today++ {
		t += x[today]
	}
	e1 := t / fp
	t = 0
	t += e1
	for i := 0; i < p-1; i, today = i+1, today+1 {
		e1 = math.FMA(x[today]-e1, k, e1)
		t += e1
	}
	e2 := t / fp
	t = 0
	t += e2
	for i := 0; i < p-1; i, today = i+1, today+1 {
		e1 = math.FMA(x[today]-e1, k, e1)
		e2 = math.FMA(e1-e2, k, e2)
		t += e2
	}
	e3 := t / fp
	out[look] = e3 + (3.0*e1 - 3.0*e2)
	for ; today < n; today++ {
		e1 = math.FMA(x[today]-e1, k, e1)
		e2 = math.FMA(e1-e2, k, e2)
		e3 = math.FMA(e2-e3, k, e3)
		out[today] = e3 + (3.0*e1 - 3.0*e2)
	}
	return out
}

// rsi is TA_RSI: Wilder's averages of gains and losses.
func rsi(x []float64, p int) []float64 {
	n := len(x)
	out := nans(n)
	if p < 1 || n <= p {
		return out
	}
	if p == 1 {
		copy(out[1:], x[1:])
		return out
	}
	inv, prev := 1.0/float64(p), x[0]
	var gain, loss float64
	step := func(v float64) {
		d := v - prev
		prev = v
		g := 0.0
		if d > 0 {
			g = d
		}
		gain += g
		loss += g - d
	}
	for i := 1; i <= p; i++ {
		step(x[i])
	}
	loss *= inv
	gain *= inv
	put := func(i int) {
		out[i] = 0
		if s := gain + loss; s > 0 {
			out[i] = 100.0 * (gain / s)
		}
	}
	put(p)
	for i := p + 1; i < n; i++ {
		loss *= float64(p - 1)
		gain *= float64(p - 1)
		step(x[i])
		loss *= inv
		gain *= inv
		put(i)
	}
	return out
}

func trueRange(h, l, prevClose float64) float64 {
	g := h - l
	if v := math.Abs(prevClose - h); v > g {
		g = v
	}
	if v := math.Abs(prevClose - l); v > g {
		g = v
	}
	return g
}

// atr is TA_ATR: Wilder smoothing of the true range, seeded with its average.
func atr(h, l, c []float64, p int) []float64 {
	n := len(c)
	out := nans(n)
	if p < 1 || n <= p {
		return out
	}
	beta := float64(p-1) / float64(p)
	alpha := 1.0 - beta
	var t float64
	for i := 1; i <= p; i++ {
		t += trueRange(h[i], l[i], c[i-1])
	}
	prev := t / float64(p)
	out[p] = prev
	for i := p + 1; i < n; i++ {
		prev = math.FMA(beta, prev, alpha*trueRange(h[i], l[i], c[i-1]))
		out[i] = prev
	}
	return out
}

// taEpsilon is TA-Lib 0.8's TA_EPSILON.
const taEpsilon = 1e-14

// adx is TA_ADX: Wilder's directional movement, DX averaged over p.
func adx(h, l, c []float64, p int) []float64 {
	n, look := len(c), 2*p-1
	out := nans(n)
	if p < 2 || n <= look {
		return out
	}
	fp, today := float64(p), 0
	ph, pl, pc := h[0], l[0], c[0]
	var mdm, pdm, tr float64
	step := func(smooth bool) float64 {
		today++
		dp := h[today] - ph
		ph = h[today]
		dm := pl - l[today]
		pl = l[today]
		if smooth {
			mdm -= mdm / fp
			pdm -= pdm / fp
		}
		if dm > 0 && dp < dm {
			mdm += dm
		} else if dp > 0 && dp > dm {
			pdm += dp
		}
		r := trueRange(ph, pl, pc)
		pc = c[today]
		return r
	}
	dx := func() (float64, bool) {
		if !(tr > 0) {
			return 0, false
		}
		mdi, pdi := 100.0*(mdm/tr), 100.0*(pdm/tr)
		s := mdi + pdi
		if -taEpsilon < s && s < taEpsilon {
			return 0, false
		}
		return 100.0 * (math.Abs(mdi-pdi) / s), true
	}
	for i := 0; i < p-1; i++ {
		tr += step(false)
	}
	var sum float64
	for i := 0; i < p; i++ {
		r := step(true)
		tr = tr - tr/fp + r
		if v, ok := dx(); ok {
			sum += v
		}
	}
	prev := sum / fp
	out[today] = prev
	for today < n-1 {
		r := step(true)
		tr = tr - tr/fp + r
		if v, ok := dx(); ok {
			prev = (prev*float64(p-1) + v) / fp
		}
		out[today] = prev
	}
	return out
}

// bbands is TA_BBANDS with an SMA middle band: a shifted running variance,
// reseeded when it loses precision.
func bbands(x []float64, p int, dev float64) (up, mid, lo []float64) {
	n, look := len(x), p-1
	up, mid, lo = nans(n), nans(n), nans(n)
	if p < 2 || n <= look {
		return
	}
	inv, trailing, reseed := 1.0/float64(p), 0, 32*p
	shiftBy := x[0]
	var ma, v1, v2 float64
	for _, v := range x[:look] {
		ma += v
		t := v - shiftBy
		v1 += t
		t *= t
		v2 += t
	}
	for i := look; i < n; i++ {
		ma += x[i]
		t := x[i] - shiftBy
		v1 += t
		t *= t
		v2 += t
		m1 := v1 * inv
		variance := v2*inv - m1*m1
		mid[i] = ma / float64(p)
		ma -= x[trailing]
		t = x[trailing] - shiftBy
		v1 -= t
		t *= t
		v2 -= t
		trailing++
		reseed--
		if variance < 0.000001*(v2*inv) || t > 1000000.0*v2 || reseed <= 0 {
			reseed, t = 32*p, 0.0
			ws := i - look
			for _, v := range x[ws : i+1] {
				t += v
			}
			shiftBy, v1, v2 = t*inv, 0, 0
			for _, v := range x[ws : i+1] {
				t = v - shiftBy
				v1 += t
				t *= t
				v2 += t
			}
			m1 = v1 * inv
			if variance = v2*inv - m1*m1; variance < 0.000000000001*(v2*inv) {
				variance = 0
			}
			t = x[ws] - shiftBy
			v1 -= t
			t *= t
			v2 -= t
		}
		sd := 0.0
		if variance != 0 {
			sd = math.Sqrt(variance)
		}
		d := sd * dev
		up[i], lo[i] = mid[i]+d, mid[i]-d
	}
	return
}

// macd is TA_MACD: fast and slow EMAs whose seed windows end on the same
// bar, and a signal EMA seeded with the first signal MACD values.
func macd(x []float64, fast, slow, signal int) (m, s, h []float64) {
	n := len(x)
	m, s, h = nans(n), nans(n), nans(n)
	if slow < fast {
		fast, slow = slow, fast
	}
	look := (signal - 1) + (slow - 1)
	if fast < 2 || signal < 1 || n <= look {
		return
	}
	fk, sk, gk := 2.0/float64(fast+1), 2.0/float64(slow+1), 2.0/float64(signal+1)
	today := 0
	var t, pf float64
	for ; today < slow-fast; today++ {
		t += x[today]
	}
	for i := 0; i < fast; i, today = i+1, today+1 {
		pf += x[today]
		t += x[today]
	}
	ps := t / float64(slow)
	pf = pf / float64(fast)
	mv := pf - ps
	sig := 0.0
	sig += mv
	for i := 0; i < signal-1; i, today = i+1, today+1 {
		pf = math.FMA(x[today]-pf, fk, pf)
		ps = math.FMA(x[today]-ps, sk, ps)
		mv = pf - ps
		sig += mv
	}
	sig = sig / float64(signal)
	m[look], s[look], h[look] = mv, sig, mv-sig
	for ; today < n; today++ {
		pf = math.FMA(x[today]-pf, fk, pf)
		ps = math.FMA(x[today]-ps, sk, ps)
		mv = pf - ps
		if signal == 1 {
			sig = mv
		} else {
			sig = math.FMA(mv-sig, gk, sig)
		}
		m[today], s[today], h[today] = mv, sig, mv-sig
	}
	return
}

// stoch is TA_STOCH with SMA smoothing: raw %K over p, slow %K over sk, %D over sd.
func stoch(h, l, c []float64, p, sk, sd int) (k, d []float64) {
	n, look := len(c), (p-1)+(sk-1)+(sd-1)
	k, d = nans(n), nans(n)
	if p < 1 || sk < 1 || sd < 1 || n <= look {
		return
	}
	raw := make([]float64, 0, n-p+1)
	trailing, lowIdx, highIdx := 0, -1, -1
	var lowest, highest float64
	for today := p - 1; today < n; today, trailing = today+1, trailing+1 {
		if lowIdx < trailing {
			lowIdx, lowest = trailing, l[trailing]
			for i := lowIdx + 1; i <= today; i++ {
				if l[i] < lowest {
					lowIdx, lowest = i, l[i]
				}
			}
		} else if l[today] <= lowest {
			lowIdx, lowest = today, l[today]
		}
		if highIdx < trailing {
			highIdx, highest = trailing, h[trailing]
			for i := highIdx + 1; i <= today; i++ {
				if h[i] > highest {
					highIdx, highest = i, h[i]
				}
			}
		} else if h[today] >= highest {
			highIdx, highest = today, h[today]
		}
		v := 0.0
		if math.Abs(highest-lowest) > taEpsilon*(math.Abs(highest)+math.Abs(lowest)) {
			v = (c[today] - lowest) / (highest - lowest) * 100.0
		}
		raw = append(raw, v)
	}
	slowK := sma(raw, sk)
	slowD := sma(slowK, sd)
	for j, v := range slowD {
		k[look+j], d[look+j] = slowK[j+sd-1], v
	}
	return
}
