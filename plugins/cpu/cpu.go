package cpu

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/customerio/monitor/metrics"
)

const (
	userGauge = iota
	systemGauge
	idleGauge
	niceGauge
)

// Hold cpu sampled data and calculate moving averages
type sample struct {
	alpha        float64
	expMovingAvg float64
	values       []float64
	position     int
	filled       bool
}

func newSample(alpha float64, ringSize float64) *sample {
	return &sample{
		alpha:  alpha,
		values: make([]float64, int64(ringSize)),
	}
}

func (s *sample) add(v float64) {
	if s.position == 0 && !s.filled {
		s.expMovingAvg = v
	} else {
		s.expMovingAvg = v*s.alpha + (s.expMovingAvg * (1 - s.alpha))
	}

	s.values[s.position] = v
	s.position = (s.position + 1) % len(s.values)
	if s.position == 0 {
		s.filled = true
	}
}

func (s *sample) movingAvg() float64 {
	if len(s.values) == 0 {
		return 0
	}
	var (
		subSet     = (len(s.values) / 2) + (len(s.values) % 2)
		toRange    = len(s.values) / 2
		avgs       []float64
		sum, count float64
	)

	// If have not filled the ring buffer yet only  get average for inserted
	// values, otherwise 0s will throw off the average.
	if !s.filled {
		subSet = (s.position / 2) + (s.position % 2)
		toRange = s.position / 2
	}

	for i := 0; i <= toRange; i++ {
		for _, v := range s.values[i : i+subSet] {
			sum += v
			count += 1
		}
		avgs = append(avgs, (sum / count))
		sum = 0
		count = 0
	}

	for _, v := range avgs {
		sum += v
		count += 1
	}

	return sum / count
}

type CPU struct {
	mux           sync.Mutex
	previous      []int
	current       []int
	currentTotal  int
	previousTotal int
	lastUpdate    time.Time
	averages      []*sample
	updaters      []metrics.Updater
	slackURL      string
}

type Config struct {
	Threshold         float64
	SampleRate        time.Duration
	ReportingInterval time.Duration
	SlackURL          string
	SlackInterval     time.Duration
	Hostname          string
}

func New(cfg *Config) *CPU {
	var (
		// Determine the alpha factor for exponential moving average as described here
		// https://en.wikipedia.org/wiki/Moving_average#Application_to_measuring_computer_performance
		alpha        = cfg.SampleRate.Seconds() / cfg.ReportingInterval.Seconds()
		ringSize     = cfg.ReportingInterval.Seconds() / cfg.SampleRate.Seconds()
		triggered    bool
		lastUpdate   time.Time
		alertCount   int
		resolveCount int
	)

	c := &CPU{
		averages: []*sample{
			userGauge:   newSample(alpha, ringSize),
			systemGauge: newSample(alpha, 1), // Moving average not used for alerts
			idleGauge:   newSample(alpha, 1), // Moving average not used for alerts
		},
		updaters: []metrics.Updater{
			userGauge:   metrics.NewGauge("cpu.user"),
			systemGauge: metrics.NewGauge("cpu.system"),
			idleGauge:   metrics.NewGauge("cpu.idle"),
		},
		slackURL: cfg.SlackURL,
	}

	go func() {
		c.collect()
		for range time.Tick(cfg.SampleRate) {
			c.collect()
			c.mux.Lock()
			for _, i := range []int{userGauge, systemGauge, idleGauge} {
				c.averages[i].add(c.rate(i))
			}
			avg := c.averages[userGauge].movingAvg()
			c.mux.Unlock()

			if avg >= cfg.Threshold {
				if alertCount < 3 {
					alertCount++
				}
				resolveCount = 0
			} else {
				if resolveCount < 3 {
					resolveCount++
				}
				alertCount = 0
			}

			if alertCount == 3 && time.Since(lastUpdate) > cfg.SlackInterval {
				triggered = true
				lastUpdate = time.Now()
				c.postSlack(cfg.Hostname, fmt.Sprintf("[ALERT]: cpu.user average utilization %f is higher than %f", avg, cfg.Threshold))
			} else if triggered && resolveCount == 3 && time.Since(lastUpdate) > cfg.SlackInterval {
				triggered = false
				lastUpdate = time.Now()
				c.postSlack(cfg.Hostname, "[RESOLVED]: cpu.user average utilization is within threshold")
			}
		}
	}()

	return c
}

func (c *CPU) postSlack(hostname, msg string) {
	if len(c.slackURL) == 0 {
		fmt.Printf("%s\n", msg)
		return
	}

	client := &http.Client{Timeout: time.Second * 10}

	type message struct {
		Text string `json:"text"`
	}
	m := message{Text: fmt.Sprintf("report from host %s\n%s", hostname, msg)}

	body, err := json.Marshal(&m)
	if err != nil {
		fmt.Printf("cpu: could not marshal message: %v: %s\n", err, msg)
		return
	}

	v := url.Values{}
	v.Set("payload", string(body))
	resp, err := client.PostForm(c.slackURL, v)
	if err != nil {
		fmt.Printf("cpu: post stack notification: %v: %s\n", err, msg)
		return
	}

	defer resp.Body.Close()
	io.Copy(ioutil.Discard, resp.Body)
}

func (c *CPU) Collect(b *metrics.Batch) {
	c.mux.Lock()
	for _, i := range []int{userGauge, systemGauge, idleGauge} {
		c.updaters[i].Update(c.averages[i].expMovingAvg)
	}
	c.mux.Unlock()
	for _, u := range c.updaters {
		u.Fill(b)
	}
}

func (c *CPU) clear() {
	c.current = nil
	c.previous = nil
	c.currentTotal = 0
	c.previousTotal = 0
}

func (c *CPU) rate(name int) float64 {
	if name >= len(c.current) {
		return 0
	}
	delta := c.current[name] - c.previous[name]
	total := c.currentTotal - c.previousTotal

	if total == 0.0 {
		return 0
	}
	return float64(delta) / float64(total) * 100
}
