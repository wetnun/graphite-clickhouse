package rollup

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"time"

	"github.com/lomik/graphite-clickhouse/helper/point"
)

/*
<graphite_rollup>
 	<pattern>
 		<regexp>click_cost</regexp>
 		<function>any</function>
 		<retention>
 			<age>0</age>
 			<precision>3600</precision>
 		</retention>
 		<retention>
 			<age>86400</age>
 			<precision>60</precision>
 		</retention>
 	</pattern>
 	<default>
 		<function>max</function>
 		<retention>
 			<age>0</age>
 			<precision>60</precision>
 		</retention>
 		<retention>
 			<age>3600</age>
 			<precision>300</precision>
 		</retention>
 		<retention>
 			<age>86400</age>
 			<precision>3600</precision>
 		</retention>
 	</default>
</graphite_rollup>
*/

type Retention struct {
	Age       int32 `xml:"age"`
	Precision int32 `xml:"precision"`
}

type Pattern struct {
	Regexp    string                      `xml:"regexp"`
	Function  string                      `xml:"function"`
	Retention []*Retention                `xml:"retention"`
	aggr      func([]point.Point) float64 `xml:"-"`
	re        *regexp.Regexp              `xml:"-"`
}

type Rollup struct {
	Pattern []*Pattern `xml:"pattern"`
	Default *Pattern   `xml:"default"`
}

func (rr *Pattern) compile(hasRegexp bool) error {
	var err error
	if hasRegexp {
		rr.re, err = regexp.Compile(rr.Regexp)
		if err != nil {
			return err
		}
	}

	aggrMap := map[string](func([]point.Point) float64){
		"avg":     AggrAvg,
		"max":     AggrMax,
		"min":     AggrMin,
		"sum":     AggrSum,
		"any":     AggrAny,
		"anyLast": AggrAnyLast,
	}

	var exists bool
	rr.aggr, exists = aggrMap[rr.Function]

	if !exists {
		return fmt.Errorf("unknown function %#v", rr.Function)
	}

	return nil
}

func (r *Rollup) compile() error {
	if r.Pattern == nil {
		r.Pattern = make([]*Pattern, 0)
	}

	if r.Default == nil {
		return fmt.Errorf("default rollup rule not set")
	}

	if err := r.Default.compile(false); err != nil {
		return err
	}

	for _, rr := range r.Pattern {
		if err := rr.compile(true); err != nil {
			return err
		}
	}

	return nil
}

func ParseRollupXML(body []byte) (*Rollup, error) {
	r := &Rollup{}
	err := xml.Unmarshal(body, r)
	if err != nil {
		return nil, err
	}

	err = r.compile()
	if err != nil {
		return nil, err
	}

	return r, nil
}

// PointsCleanup removes points with empty metric
// for run after Deduplicate, Merge, etc for result cleanup
func PointsCleanup(points []point.Point) []point.Point {
	l := len(points)
	squashed := 0

	for i := 0; i < l; i++ {
		if points[i].Metric == "" {
			squashed++
			continue
		}
		if squashed > 0 {
			points[i-squashed] = points[i]
		}
	}

	return points[:l-squashed]
}

// PointsUniq removes points with equal metric and time
func PointsUniq(points []point.Point) []point.Point {
	l := len(points)
	var i, n int
	// i - current position of iterator
	// n - position on first record with current key (metric + time)

	for i = 1; i < l; i++ {
		if points[i].Metric != points[n].Metric ||
			points[i].Time != points[n].Time {
			n = i
			continue
		}

		if points[i].Timestamp > points[n].Timestamp {
			points[n] = points[i]
		}

		points[i].Metric = "" // mark for remove
	}

	return PointsCleanup(points)
}

// Match returns rollup rules for metric
func (r *Rollup) Match(metric string) *Pattern {
	for _, rr := range r.Pattern {
		if rr.re.MatchString(metric) {
			return rr
		}
	}

	return r.Default
}

func doMetricPrecision(points []point.Point, precision int32, aggr func([]point.Point) float64) []point.Point {
	l := len(points)
	var i, n int
	// i - current position of iterator
	// n - position of the first record with time rounded to precision

	if l == 0 {
		return points
	}

	// set first point time
	t := points[0].Time
	t = t - (t % precision)
	points[0].Time = t

	for i = 1; i < l; i++ {
		t = points[i].Time
		t = t - (t % precision)
		points[i].Time = t

		if points[n].Time == t {
			points[i].Metric = ""
		} else {
			if i > n+1 {
				points[n].Value = aggr(points[n:i])
			}
			n = i
		}
	}
	if i > n+1 {
		points[n].Value = aggr(points[n:i])
	}

	return PointsCleanup(points)
}

// RollupMetric rolling up list of points of ONE metric sorted by key "time"
// returns (new points slice, precision)
func (r *Rollup) RollupMetric(points []point.Point) ([]point.Point, int32) {
	// pp.Println(points)

	l := len(points)
	if l == 0 {
		return points, 1
	}

	now := int32(time.Now().Unix())
	rule := r.Match(points[0].Metric)
	precision := int32(1)

	for _, retention := range rule.Retention {
		if points[0].Time > now-retention.Age && retention.Age != 0 {
			break
		}

		points = doMetricPrecision(points, retention.Precision, rule.aggr)
		precision = retention.Precision
	}

	// pp.Println(points)
	return points, precision
}
