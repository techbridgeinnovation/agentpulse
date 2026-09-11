package recorder

import "time"

const (
	defaultQueueSize   = 2048
	defaultBatchSize   = 100
	defaultFlushEvery  = 2 * time.Second
	defaultSendTimeout = 10 * time.Second
	minimumQueueSize   = 1

	// defaultRateRefresh is how often the rate card is read again.
	//
	// A rate card is a price list a person maintains, so it changes rarely and a new entry only has to arrive before an agent needs it. This bounds how long a model added to the card stays unpriced here, at the cost of one small read per process per quarter of an hour.
	defaultRateRefresh = 15 * time.Minute
)

// Config describes where records go and how much memory the recorder may use
// holding them.
//
// Every field has a working default, so recorder.New(recorder.Config{}) is
// valid and inert. There is deliberately no option to disable the dropped
// counter or to make the queue unbounded: an unbounded queue turns a slow sink
// into the host agent's memory leak.
type Config struct {
	// Sinks receive every record. Each runs independently, so one failing does
	// not affect the others. Empty means discard.
	Sinks []Sink

	// QueueSize is how many records may wait for delivery. Once full, new
	// records are dropped and counted rather than blocking the agent.
	QueueSize int

	// BatchSize is the most records handed to a sink at once.
	BatchSize int

	// FlushEvery bounds how long a record waits before delivery, so a quiet
	// agent still reports.
	FlushEvery time.Duration

	// SendTimeout caps one delivery attempt, so a hung sink cannot stall the
	// worker for every other sink behind it. It caps one rate card fetch too.
	SendTimeout time.Duration

	// Rates supplies the prices the per-request running total is measured against, fetched in the background and never on the path of a model call.
	//
	// Optional. Without one, every record contributes nothing to the running total and SpentOn answers zero, the same way an empty Sinks list records nothing. A spend ceiling read from SpentOn needs one; nothing else does, because metering prices what it stores whatever this is set to.
	Rates RateSource

	// RateRefresh is how often the rate card is fetched again. Defaults to defaultRateRefresh.
	RateRefresh time.Duration
}

func (c Config) withDefaults() Config {
	if len(c.Sinks) == 0 {
		c.Sinks = []Sink{Discard()}
	}
	if c.QueueSize < minimumQueueSize {
		c.QueueSize = defaultQueueSize
	}
	if c.BatchSize < 1 {
		c.BatchSize = defaultBatchSize
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = defaultFlushEvery
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = defaultSendTimeout
	}
	if c.RateRefresh <= 0 {
		c.RateRefresh = defaultRateRefresh
	}
	return c
}
