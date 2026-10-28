package shell

import (
	"encoding/json"
	"time"

	"github.com/graybeardcorp/weft/chart"
)

// DecodeCronSeries reads the /api/crons/{name}/chart response. Shared by
// the CLI backends and the browser shell's FS.
func DecodeCronSeries(data []byte) (chart.CronSeries, error) {
	var resp struct {
		Granularity string `json:"granularity"`
		Start       int64  `json:"start"`
		End         int64  `json:"end"`
		Points      []struct {
			Ts    int64   `json:"ts"`
			Tag   string  `json:"tag"`
			Count int64   `json:"count"`
			Sum   float64 `json:"sum"`
			Min   float64 `json:"min"`
			Max   float64 `json:"max"`
		} `json:"points"`
		Alert *chart.CronAlert `json:"alert"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return chart.CronSeries{}, err
	}
	out := chart.CronSeries{
		Granularity: resp.Granularity,
		Start:       time.UnixMilli(resp.Start),
		End:         time.UnixMilli(resp.End),
		Buckets:     make([]chart.CronBucket, len(resp.Points)),
		Alert:       resp.Alert,
	}
	for i, p := range resp.Points {
		out.Buckets[i] = chart.CronBucket{Start: time.UnixMilli(p.Ts), Tag: p.Tag, Count: p.Count, Sum: p.Sum, Min: p.Min, Max: p.Max}
	}
	return out, nil
}
