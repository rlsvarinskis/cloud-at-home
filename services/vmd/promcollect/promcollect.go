// Package promcollect exports vmd's operational state as Prometheus metrics. The Collector is a
// prometheus.Collector: capacity and VM counts are computed on every scrape, guest metrics come from a
// sampler that caches the last good sample per VM (so a scrape never calls a node), and reconciler
// activity is counted through the hooks returned by Hooks.
//
// Every metric name starts with attic_. Label values are limited to identifiers the control plane
// already knows (node, VM ID, project, VM name, disk ID, device and mount point); free-form text such as
// command lines is never exported.
package promcollect

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	vmdv1 "github.com/rlsvarinskis/cloud-at-home/api/gen/attic/vmd/v1"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/scheduler"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/service"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
)

// Options tunes the sampler. A zero field takes its default.
type Options struct {
	// SampleInterval is how often running VMs are sampled (default 30s). A VM whose last good sample is
	// older than three intervals stops being exported.
	SampleInterval time.Duration
	// SampleTimeout bounds the sample of one VM (default 10s).
	SampleTimeout time.Duration
	// Concurrency is how many VMs are sampled in parallel (default 8).
	Concurrency int
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Log receives warnings from scrapes and sampling passes (default slog.Default()).
	Log *slog.Logger
}

const (
	defaultSampleInterval = 30 * time.Second
	defaultSampleTimeout  = 10 * time.Second
	defaultConcurrency    = 8

	// staleSampleIntervals is how many sampling intervals a VM's last good sample stays exported.
	staleSampleIntervals = 3
	// scrapeTimeout bounds the store reads made by one scrape.
	scrapeTimeout = 10 * time.Second

	sourceCapacity = "capacity"
	outcomeOK      = "ok"
	outcomeError   = "error"
)

func (o Options) withDefaults() Options {
	if o.SampleInterval <= 0 {
		o.SampleInterval = defaultSampleInterval
	}
	if o.SampleTimeout <= 0 {
		o.SampleTimeout = defaultSampleTimeout
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return o
}

// Collector implements prometheus.Collector. Create one with New, register it once, and drive the
// sampler with Run (or SampleOnce).
type Collector struct {
	st    store.Store
	nodes service.Nodes
	sched scheduler.Config
	opts  Options
	log   *slog.Logger

	// sampleMu serialises sampling passes; mu guards cache, which Collect reads while a pass writes.
	sampleMu sync.Mutex
	mu       sync.Mutex
	cache    map[string]*cachedSample

	// Self-instrumentation and reconciler metrics. They are not described or collected by name; the
	// Collector forwards Describe and Collect to every one of them (see inner).
	scrapeErrors        *prometheus.CounterVec
	sampleErrors        prometheus.Counter
	sampleDuration      prometheus.Gauge
	sampleTimestamp     prometheus.Gauge
	reconcileVM         *prometheus.CounterVec
	reconcileVMDuration prometheus.Histogram
	ticks               prometheus.Counter
	tickDuration        prometheus.Histogram
	lastTickClaimed     prometheus.Gauge

	inner []prometheus.Collector
}

// cachedSample is the last successful sample of one VM, with the labels it was taken under.
type cachedSample struct {
	vmID, project, name, node string
	at                        time.Time
	m                         *vmdv1.VMMetrics
}

// New returns a Collector that reads capacity from st and nodes, and computes capacity with sched.
// Register it with a registry (reg.MustRegister(c)) and start the sampler with Run.
func New(st store.Store, nodes service.Nodes, sched scheduler.Config, opts Options) *Collector {
	opts = opts.withDefaults()
	c := &Collector{
		st:    st,
		nodes: nodes,
		sched: sched,
		opts:  opts,
		log:   opts.Log.With("component", "promcollect"),
		cache: make(map[string]*cachedSample),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "attic_vmd_scrape_errors_total",
			Help: "Scrapes that could not read a source, by source.",
		}, []string{"source"}),
		sampleErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "attic_vm_sample_errors_total",
			Help: "Samples of running VMs that failed.",
		}),
		sampleDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "attic_vm_samples_last_duration_seconds",
			Help: "How long the last sampling pass took.",
		}),
		sampleTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "attic_vm_samples_last_timestamp_seconds",
			Help: "Unix time at which the last sampling pass finished.",
		}),
		reconcileVM: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "attic_reconcile_vm_total",
			Help: "VMs reconciled, by outcome.",
		}, []string{"outcome"}),
		reconcileVMDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "attic_reconcile_vm_duration_seconds",
			Help: "Time spent reconciling one VM.",
		}),
		ticks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "attic_reconcile_ticks_total",
			Help: "Reconciler ticks completed.",
		}),
		tickDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "attic_reconcile_tick_duration_seconds",
			Help: "Time spent in one reconciler tick.",
		}),
		lastTickClaimed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "attic_reconcile_last_tick_claimed",
			Help: "VMs claimed by the last reconciler tick.",
		}),
	}
	// Create the label values up front so the series exist from the first scrape.
	c.scrapeErrors.WithLabelValues(sourceCapacity)
	c.reconcileVM.WithLabelValues(outcomeOK)
	c.reconcileVM.WithLabelValues(outcomeError)
	c.inner = []prometheus.Collector{c.scrapeErrors, c.sampleErrors, c.sampleDuration, c.sampleTimestamp,
		c.reconcileVM, c.reconcileVMDuration, c.ticks, c.tickDuration, c.lastTickClaimed}
	return c
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range ownDescs {
		ch <- d
	}
	for _, m := range c.inner {
		m.Describe(ch)
	}
}

// Collect implements prometheus.Collector. It reads the store for capacity and VM counts, and the
// sampler cache for guest metrics; it never contacts a node.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.collectCapacity(ch)
	c.collectVMs(ch)
	for _, m := range c.inner {
		m.Collect(ch)
	}
}

// Run samples immediately and then every SampleInterval until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	c.SampleOnce(ctx)
	t := time.NewTicker(c.opts.SampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.SampleOnce(ctx)
		}
	}
}

// Hooks returns reconciler hooks that count and time reconciliation. Pass them to
// Reconciler.Hooks.
func (c *Collector) Hooks() service.Hooks {
	return service.Hooks{
		OnVMReconciled: func(_ *domain.VM, d time.Duration, err error) {
			outcome := outcomeOK
			if err != nil {
				outcome = outcomeError
			}
			c.reconcileVM.WithLabelValues(outcome).Inc()
			c.reconcileVMDuration.Observe(d.Seconds())
		},
		OnTick: func(claimed int, d time.Duration) {
			c.ticks.Inc()
			c.tickDuration.Observe(d.Seconds())
			c.lastTickClaimed.Set(float64(claimed))
		},
	}
}

// boolFloat maps a condition to the 1 or 0 used by the gauges.
func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// gauge sends one gauge sample. A label value the exposition format cannot carry (invalid UTF-8) drops
// that sample and logs it rather than failing the whole scrape.
func (c *Collector) gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	m, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, v, labels...)
	if err != nil {
		c.log.Warn("drop metric", "err", err)
		return
	}
	ch <- m
}
