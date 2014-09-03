package expr

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/StackExchange/bosun/_third_party/github.com/GaryBoone/GoStats/stats"
	"github.com/StackExchange/bosun/_third_party/github.com/MiniProfiler/go/miniprofiler"
	"github.com/StackExchange/bosun/_third_party/github.com/StackExchange/scollector/opentsdb"
	"github.com/StackExchange/bosun/expr/parse"
	"github.com/StackExchange/bosun/search"
)

var builtins = map[string]parse.Func{
	"abs": {
		[]parse.FuncType{parse.TYPE_NUMBER},
		parse.TYPE_NUMBER,
		Abs,
	},
	"avg": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Avg,
	},
	"sum": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Sum,
	},
	"band": {
		[]parse.FuncType{parse.TYPE_STRING, parse.TYPE_STRING, parse.TYPE_STRING, parse.TYPE_SCALAR},
		parse.TYPE_SERIES,
		Band,
	},
	"count": {
		[]parse.FuncType{parse.TYPE_STRING, parse.TYPE_STRING, parse.TYPE_STRING},
		parse.TYPE_SCALAR,
		Count,
	},
	"change": {
		[]parse.FuncType{parse.TYPE_STRING, parse.TYPE_STRING, parse.TYPE_STRING},
		parse.TYPE_NUMBER,
		Change,
	},
	"diff": {
		[]parse.FuncType{parse.TYPE_STRING, parse.TYPE_STRING, parse.TYPE_STRING},
		parse.TYPE_NUMBER,
		Diff,
	},
	"dev": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Dev,
	},
	"first": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		First,
	},
	"len": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Length,
	},
	"last": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Last,
	},
	"min": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Min,
	},
	"max": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Max,
	},
	"since": {
		[]parse.FuncType{parse.TYPE_SERIES},
		parse.TYPE_NUMBER,
		Since,
	},
	"forecastlr": {
		[]parse.FuncType{parse.TYPE_SERIES, parse.TYPE_SCALAR},
		parse.TYPE_NUMBER,
		Forecast_lr,
	},
	"percentile": {
		[]parse.FuncType{parse.TYPE_SERIES, parse.TYPE_SCALAR},
		parse.TYPE_NUMBER,
		Percentile,
	},
	"q": {
		[]parse.FuncType{parse.TYPE_STRING, parse.TYPE_STRING, parse.TYPE_STRING},
		parse.TYPE_SERIES,
		Query,
	},
	"regroup": {
		[]parse.FuncType{parse.TYPE_NUMBER, parse.TYPE_STRING},
		parse.TYPE_NUMBER,
		Regroup,
	},
	"t": {
		[]parse.FuncType{parse.TYPE_NUMBER, parse.TYPE_STRING},
		parse.TYPE_SERIES,
		Transpose,
	},
	"ungroup": {
		[]parse.FuncType{parse.TYPE_NUMBER},
		parse.TYPE_SCALAR,
		Ungroup,
	},
	"lookup": {
		[]parse.FuncType{parse.TYPE_STRING, parse.TYPE_STRING},
		parse.TYPE_NUMBER,
		lookup,
	},
}

func lookup(e *state, T miniprofiler.Timer, lookup, key string) (results *Results, err error) {
	results = new(Results)
	results.IgnoreUnjoined = true
	lookups := e.lookups[lookup]
	if lookups == nil {
		err = fmt.Errorf("lookup table not found: %v", lookup)
		return
	}
	var tags []opentsdb.TagSet
	for _, tag := range lookups.Tags {
		var next []opentsdb.TagSet
		for _, value := range e.search.TagValuesByTagKey(tag) {
			for _, s := range tags {
				t := s.Copy()
				t[tag] = value
				next = append(next, t)
			}
			if len(tags) == 0 {
				next = append(next, opentsdb.TagSet{tag: value})
			}
		}
		tags = next
	}
	for _, tag := range tags {
		for _, entry := range lookups.Entries {
			value, ok := entry.Values[key]
			if !ok {
				continue
			}
			match := true
			for ak, av := range entry.AlertKey.Group() {
				matches, err := search.Match(av, []string{tag[ak]})
				if err != nil {
					return nil, err
				}
				if len(matches) == 0 {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			var num float64
			num, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, err
			}
			results.Results = append(results.Results, &Result{
				Value: Number(num),
				Group: tag,
			})
			break
		}
	}
	return results, nil
}

func Band(e *state, T miniprofiler.Timer, query, duration, period string, num float64) (r *Results, err error) {
	r = new(Results)
	r.IgnoreOtherUnjoined = true
	T.Step("band", func(T miniprofiler.Timer) {
		var d, p opentsdb.Duration
		d, err = opentsdb.ParseDuration(duration)
		if err != nil {
			return
		}
		p, err = opentsdb.ParseDuration(period)
		if err != nil {
			return
		}
		if num < 1 || num > 100 {
			err = fmt.Errorf("expr: Band: num out of bounds")
		}
		q, err := opentsdb.ParseQuery(query)
		if q == nil && err != nil {
			return
		}
		if err = e.search.Expand(q); err != nil {
			return
		}
		req := opentsdb.Request{
			Queries: []*opentsdb.Query{q},
		}
		now := e.now
		req.End = now.Unix()
		req.Start = now.Add(time.Duration(-d)).Unix()
		if err = req.SetTime(e.now); err != nil {
			return
		}
		for i := 0; i < int(num); i++ {
			now = now.Add(time.Duration(-p))
			req.End = now.Unix()
			req.Start = now.Add(time.Duration(-d)).Unix()
			var s opentsdb.ResponseSet
			s, err = timeRequest(e, T, &req)
			if err != nil {
				return
			}
			for _, res := range s {
				newarr := true
				for _, a := range r.Results {
					if !a.Group.Equal(res.Tags) {
						continue
					}
					newarr = false
					values := a.Value.(Series)
					for k, v := range res.DPS {
						values[k] = v
					}
				}
				if newarr {
					values := make(Series)
					a := &Result{Group: res.Tags}
					for k, v := range res.DPS {
						values[k] = v
					}
					a.Value = values
					r.Results = append(r.Results, a)
				}
			}
		}
	})
	return
}

func Query(e *state, T miniprofiler.Timer, query, sduration, eduration string) (r *Results, err error) {
	r = new(Results)
	q, err := opentsdb.ParseQuery(query)
	if q == nil && err != nil {
		return
	}
	if err = e.search.Expand(q); err != nil {
		return
	}
	sd, err := opentsdb.ParseDuration(sduration)
	if err != nil {
		return
	}
	req := opentsdb.Request{
		Queries: []*opentsdb.Query{q},
		Start:   fmt.Sprintf("%s-ago", sd),
	}
	if eduration != "" {
		var ed opentsdb.Duration
		ed, err = opentsdb.ParseDuration(eduration)
		if err != nil {
			return
		}
		req.End = fmt.Sprintf("%s-ago", ed)
	}
	var s opentsdb.ResponseSet
	if err = req.SetTime(e.now); err != nil {
		return
	}
	s, err = timeRequest(e, T, &req)
	if err != nil {
		return
	}
	for _, res := range s {
		r.Results = append(r.Results, &Result{
			Value: Series(res.DPS),
			Group: res.Tags,
		})
	}
	return
}

func timeRequest(e *state, T miniprofiler.Timer, req *opentsdb.Request) (s opentsdb.ResponseSet, err error) {
	r := *req
	if e.autods > 0 {
		if err := r.AutoDownsample(e.autods); err != nil {
			return nil, err
		}
	}
	e.addRequest(r)
	b, _ := json.MarshalIndent(&r, "", "  ")
	T.StepCustomTiming("tsdb", "query", string(b), func() {
		s, err = e.context.Query(&r)
	})
	return
}

func Change(e *state, T miniprofiler.Timer, query, sduration, eduration string) (r *Results, err error) {
	r = new(Results)
	sd, err := opentsdb.ParseDuration(sduration)
	if err != nil {
		return
	}
	var ed opentsdb.Duration
	if eduration != "" {
		ed, err = opentsdb.ParseDuration(eduration)
		if err != nil {
			return
		}
	}
	r, err = Query(e, T, query, sduration, eduration)
	if err != nil {
		return
	}
	r, err = reduce(e, T, r, change, (sd - ed).Seconds())
	return
}

func change(dps Series, args ...float64) float64 {
	return avg(dps) * args[0]
}

func Diff(e *state, T miniprofiler.Timer, query, sduration, eduration string) (r *Results, err error) {
	r, err = Query(e, T, query, sduration, eduration)
	if err != nil {
		return
	}
	r, err = reduce(e, T, r, diff)
	return
}

func diff(dps Series, args ...float64) float64 {
	return last(dps) - first(dps)
}

func reduce(e *state, T miniprofiler.Timer, series *Results, F func(Series, ...float64) float64, args ...float64) (*Results, error) {
	res := *series
	res.Results = nil
	for _, s := range series.Results {
		switch t := s.Value.(type) {
		case Series:
			if len(t) == 0 {
				continue
			}
			s.Value = Number(F(t, args...))
			res.Results = append(res.Results, s)
		default:
			panic(fmt.Errorf("expr: expected a series"))
		}
	}
	return &res, nil
}

func Abs(e *state, T miniprofiler.Timer, series *Results) *Results {
	for _, s := range series.Results {
		s.Value = Number(math.Abs(float64(s.Value.Value().(Number))))
	}
	return series
}

func Avg(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, avg)
}

// avg returns the mean of x.
func avg(dps Series, args ...float64) (a float64) {
	for _, v := range dps {
		a += float64(v)
	}
	a /= float64(len(dps))
	return
}

func Count(e *state, T miniprofiler.Timer, query, sduration, eduration string) (r *Results, err error) {
	r, err = Query(e, T, query, sduration, eduration)
	if err != nil {
		return
	}
	return &Results{
		Results: []*Result{
			{Value: Scalar(len(r.Results))},
		},
	}, nil
}

func Sum(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, sum)
}

func sum(dps Series, args ...float64) (a float64) {
	for _, v := range dps {
		a += float64(v)
	}
	return
}

func Dev(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, dev)
}

// dev returns the sample standard deviation of x.
func dev(dps Series, args ...float64) (d float64) {
	a := avg(dps)
	for _, v := range dps {
		d += math.Pow(float64(v)-a, 2)
	}
	// how should we handle len(x) == 1?
	d /= float64(len(dps) - 1)
	return math.Sqrt(d)
}

func Length(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, length)
}

func length(dps Series, args ...float64) (a float64) {
	return float64(len(dps))
}

func Last(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, last)
}

func last(dps Series, args ...float64) (a float64) {
	last := -1
	for k, v := range dps {
		d, err := strconv.Atoi(k)
		if err != nil {
			panic(err)
		}
		if d > last {
			a = float64(v)
			last = d
		}
	}
	return
}

func First(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, first)
}

func first(dps Series, args ...float64) (a float64) {
	first := math.MaxInt64
	for k, v := range dps {
		d, err := strconv.Atoi(k)
		if err != nil {
			panic(err)
		}
		if d < first {
			a = float64(v)
			first = d
		}
	}
	return
}

func Since(e *state, T miniprofiler.Timer, series *Results) (*Results, error) {
	return reduce(e, T, series, since)
}

func since(dps Series, args ...float64) (a float64) {
	var last time.Time
	for k := range dps {
		d, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			panic(err)
		}
		t := time.Unix(d, 0)
		if t.After(last) {
			last = t
		}
	}
	s := time.Since(last)
	return s.Seconds()
}

func Forecast_lr(e *state, T miniprofiler.Timer, series *Results, y float64) (r *Results, err error) {
	return reduce(e, T, series, forecast_lr, y)
}

// forecast_lr returns the number of seconds a linear regression predicts the
// series will take to reach y_val.
func forecast_lr(dps Series, args ...float64) float64 {
	const tenYears = time.Hour * 24 * 365 * 10
	yVal := args[0]
	var x []float64
	var y []float64
	for k, v := range dps {
		d, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			panic(err)
		}
		x = append(x, float64(d))
		y = append(y, float64(v))
	}
	var slope, intercept, _, _, _, _ = stats.LinearRegression(x, y)
	it := (yVal - intercept) / slope
	var i64 int64
	if it < math.MinInt64 {
		i64 = math.MinInt64
	} else if it > math.MaxInt64 {
		i64 = math.MaxInt64
	} else if math.IsNaN(it) {
		i64 = time.Now().Unix()
	} else {
		i64 = int64(it)
	}
	t := time.Unix(i64, 0)
	s := -time.Since(t)
	if s < -tenYears {
		s = -tenYears
	} else if s > tenYears {
		s = tenYears
	}
	return s.Seconds()
}

func Percentile(e *state, T miniprofiler.Timer, series *Results, p float64) (r *Results, err error) {
	return reduce(e, T, series, percentile, p)
}

func Min(e *state, T miniprofiler.Timer, series *Results) (r *Results, err error) {
	return reduce(e, T, series, percentile, 0)
}

func Max(e *state, T miniprofiler.Timer, series *Results) (r *Results, err error) {
	return reduce(e, T, series, percentile, 1)
}

// percentile returns the value at the corresponding percentile between 0 and 1.
// Min and Max can be simulated using p <= 0 and p >= 1, respectively.
func percentile(dps Series, args ...float64) (a float64) {
	p := args[0]
	var x []float64
	for _, v := range dps {
		x = append(x, float64(v))
	}
	sort.Float64s(x)
	if p <= 0 {
		return x[0]
	}
	if p >= 1 {
		return x[len(x)-1]
	}
	i := p * float64(len(x)-1)
	i = math.Ceil(i)
	return x[int(i)]
}

func Ungroup(e *state, T miniprofiler.Timer, d *Results) (*Results, error) {
	if len(d.Results) != 1 {
		return nil, fmt.Errorf("ungroup: requires exactly one group")
	}
	d.Results[0].Group = nil
	return d, nil
}

func Regroup(e *state, T miniprofiler.Timer, d *Results, gp string) (*Results, error) {
	m := make(map[string]bool)
	for _, g := range strings.Split(gp, ",") {
		m[g] = true
	}
	for _, v := range d.Results {
		for k := range v.Group {
			if !m[k] {
				delete(v.Group, k)
			}
		}
	}
	return d, nil
}

func Transpose(e *state, T miniprofiler.Timer, d *Results, gp string) (*Results, error) {
	gps := strings.Split(gp, ",")
	m := make(map[string]*Result)
	for _, v := range d.Results {
		ts := make(opentsdb.TagSet)
		for k, v := range v.Group {
			for _, b := range gps {
				if k == b {
					ts[k] = v
				}
			}
		}
		if _, ok := m[ts.String()]; !ok {
			m[ts.String()] = &Result{
				Group: ts,
				Value: make(Series),
			}
		}
		switch t := v.Value.(type) {
		case Number:
			r := m[ts.String()]
			i := strconv.Itoa((len(r.Value.(Series))))
			r.Value.(Series)[i] = opentsdb.Point(t)
			r.Computations = append(r.Computations, v.Computations...)
		default:
			panic(fmt.Errorf("expr: expected a number"))
		}
	}
	var r Results
	for _, res := range m {
		r.Results = append(r.Results, res)
	}
	return &r, nil
}
