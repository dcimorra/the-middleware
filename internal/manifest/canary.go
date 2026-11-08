package manifest

import (
	"fmt"
	"time"
)

// Canary brings a new generation in on part of the traffic: once it is
// healthy it gets Percent of the visitors for For, and the rest of them
// stay on the generation that was serving. If it answers more than
// MaxErrorRate of its requests with an error (5xx, or no answer), and
// more than the serving generation does, it is stopped and the serving
// one keeps everything; otherwise it takes over as a normal handover
// would. A visitor stays on the side they landed on (a cookie), so the
// pages and the assets they load come from one generation.
type Canary struct {
	Percent      int      `json:"percent,omitempty"`
	For          Duration `json:"for,omitempty"`
	MaxErrorRate float64  `json:"max_error_rate,omitempty"`
	// MinRequests is how many requests the canary must have answered for
	// its rate to mean anything; with fewer by the end it takes over,
	// since there was not enough traffic to judge it either way.
	MinRequests int `json:"min_requests,omitempty"`
}

// The defaults: a tenth of the visitors for ten minutes, rolled back
// over 5% errors once it has answered twenty requests.
const (
	DefaultCanaryPercent  = 10
	DefaultCanaryFor      = 10 * time.Minute
	DefaultCanaryMaxError = 0.05
	DefaultCanaryMinReqs  = 20
)

// Share is the percentage of visitors sent to the canary.
func (c *Canary) Share() int {
	if c.Percent == 0 {
		return DefaultCanaryPercent
	}
	return c.Percent
}

// Window is how long the canary runs before it takes over.
func (c *Canary) Window() time.Duration { return c.For.Or(DefaultCanaryFor) }

// MaxErrors is the error rate over which it is rolled back.
func (c *Canary) MaxErrors() float64 {
	if c.MaxErrorRate == 0 {
		return DefaultCanaryMaxError
	}
	return c.MaxErrorRate
}

// Enough is the requests it needs before its rate is judged.
func (c *Canary) Enough() int64 {
	if c.MinRequests == 0 {
		return DefaultCanaryMinReqs
	}
	return int64(c.MinRequests)
}

func (c *Canary) valid(field string, port int, inService bool) error {
	if c == nil {
		return nil
	}
	switch {
	case inService:
		return fmt.Errorf("%s: a service hands over whole; canary is for a site's run", field)
	case port != 0:
		// Two generations cannot share a written port, so there would be
		// nothing to send the canary's share to.
		return fmt.Errorf("%s: needs a port the house assigns (no run.port)", field)
	case c.Percent < 0 || c.Percent > 99:
		return fmt.Errorf("%s.percent: between 1 and 99", field)
	case c.For != 0 && (time.Duration(c.For) < 10*time.Second || time.Duration(c.For) > 24*time.Hour):
		return fmt.Errorf("%s.for: between 10s and 24h", field)
	case c.MaxErrorRate < 0 || c.MaxErrorRate >= 1:
		return fmt.Errorf("%s.max_error_rate: a share between 0 and 1, such as 0.05", field)
	case c.MinRequests < 0:
		return fmt.Errorf("%s.min_requests: not negative", field)
	}
	return nil
}
