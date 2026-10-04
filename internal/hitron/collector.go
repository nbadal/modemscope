package hitron

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "modemscope"

// Collector scrapes the modem on each Prometheus scrape.
//
// The modem is polled inline rather than on a background ticker: the scrape
// takes ~200ms, and polling in lockstep with Prometheus means the sample
// timestamp matches when the value was actually read.
type Collector struct {
	client *Client
	log    *slog.Logger
	budget time.Duration

	// The modem's embedded server is fragile, so overlapping scrapes (two
	// Prometheus replicas, or a human curling /metrics mid-scrape) must not each
	// fan out into their own six requests. mu serializes them and ttl lets the
	// waiter reuse the in-flight result instead of re-fetching. Rejecting the
	// second scrape instead (promhttp's MaxRequestsInFlight) would protect the
	// modem but report the exporter as down, which is a worse lie than a
	// few-seconds-old sample.
	mu         sync.Mutex
	ttl        time.Duration
	lastAt     time.Time
	lastStatus *Status
	lastErr    error

	up             *prometheus.Desc
	scrapeDuration *prometheus.Desc
	info           *prometheus.Desc
	uptime         *prometheus.Desc

	dsSNR        *prometheus.Desc
	dsPower      *prometheus.Desc
	dsFreq       *prometheus.Desc
	dsOctets     *prometheus.Desc
	dsCorrected  *prometheus.Desc
	dsUncorrect  *prometheus.Desc
	dsChannels   *prometheus.Desc
	usPower      *prometheus.Desc
	usFreq       *prometheus.Desc
	usBandwidth  *prometheus.Desc
	usChannels   *prometheus.Desc
	initState    *prometheus.Desc
	networkAcces *prometheus.Desc

	ofdmLocked      *prometheus.Desc
	ofdmLockState   *prometheus.Desc
	ofdmSNR         *prometheus.Desc
	ofdmPower       *prometheus.Desc
	ofdmFreq        *prometheus.Desc
	ofdmOctets      *prometheus.Desc
	ofdmCorrected   *prometheus.Desc
	ofdmUncorrect   *prometheus.Desc
	usOFDMEnabled   *prometheus.Desc
	usOFDMFreq      *prometheus.Desc
	usOFDMPower     *prometheus.Desc
	usOFDMChannelBw *prometheus.Desc

	linkUp         *prometheus.Desc
	linkSpeed      *prometheus.Desc
	linkFullDuplex *prometheus.Desc
}

// NewCollector returns a Collector reading from client.
//
// budget caps the total time spent talking to the modem during one scrape. It
// must stay below Prometheus's scrape timeout: if the modem is slow rather than
// dead, Prometheus giving up first means modemscope_up=0 never gets delivered —
// the "modem is sick" signal is lost precisely when it matters.
func NewCollector(client *Client, log *slog.Logger, budget time.Duration) *Collector {
	dsLabels := []string{"channel", "port"}
	usLabels := []string{"channel", "port"}

	return &Collector{
		client: client,
		log:    log,
		budget: budget,
		// Well under any sane scrape interval, so each scrape still reads the
		// modem afresh; long enough that concurrent scrapes coalesce.
		ttl: 5 * time.Second,

		up: prometheus.NewDesc(namespace+"_up",
			"1 if the modem status endpoints were scraped successfully.", nil, nil),
		scrapeDuration: prometheus.NewDesc(namespace+"_scrape_duration_seconds",
			"Time taken to scrape the modem.", nil, nil),
		info: prometheus.NewDesc(namespace+"_info",
			"Modem identity. Always 1.",
			[]string{"model", "vendor", "hw_version", "sw_version", "serial", "rf_mac", "config_name"}, nil),
		uptime: prometheus.NewDesc(namespace+"_uptime_seconds",
			"Modem uptime. A drop means the modem rebooted, which also resets every error counter below.",
			nil, nil),

		dsSNR: prometheus.NewDesc(namespace+"_downstream_snr_db",
			"Downstream signal-to-noise ratio (dB). Below ~33 dB risks uncorrectable errors on 256QAM.",
			dsLabels, nil),
		dsPower: prometheus.NewDesc(namespace+"_downstream_power_dbmv",
			"Downstream received power (dBmV). Healthy range is roughly -7..+7.", dsLabels, nil),
		dsFreq: prometheus.NewDesc(namespace+"_downstream_frequency_hz",
			"Downstream channel centre frequency (Hz).", dsLabels, nil),
		dsOctets: prometheus.NewDesc(namespace+"_downstream_octets_total",
			"Downstream octets received. Resets when the modem reboots.", dsLabels, nil),
		dsCorrected: prometheus.NewDesc(namespace+"_downstream_correcteds_total",
			"FEC-corrected codewords. Resets when the modem reboots.", dsLabels, nil),
		dsUncorrect: prometheus.NewDesc(namespace+"_downstream_uncorrectables_total",
			"Uncorrectable codewords — the leading indicator of plant trouble. Resets when the modem reboots.",
			dsLabels, nil),
		dsChannels: prometheus.NewDesc(namespace+"_downstream_channels",
			"Number of bonded downstream channels. A drop means channels fell off.", nil, nil),

		usPower: prometheus.NewDesc(namespace+"_upstream_power_dbmv",
			"Upstream transmit power (dBmV). Healthy range is roughly 35..51; sustained highs mean the modem is straining.",
			usLabels, nil),
		usFreq: prometheus.NewDesc(namespace+"_upstream_frequency_hz",
			"Upstream channel centre frequency (Hz).", usLabels, nil),
		usBandwidth: prometheus.NewDesc(namespace+"_upstream_bandwidth_hz",
			"Upstream channel bandwidth (Hz).", usLabels, nil),
		usChannels: prometheus.NewDesc(namespace+"_upstream_channels",
			"Number of bonded upstream channels.", nil, nil),

		initState: prometheus.NewDesc(namespace+"_docsis_init_state",
			"DOCSIS registration stage: 1 if healthy, 0 otherwise. A stage flipping to 0 means the modem is re-registering.",
			[]string{"stage"}, nil),
		networkAcces: prometheus.NewDesc(namespace+"_network_access",
			"1 if the CMTS permits the modem on the network.", nil, nil),

		// DOCSIS 3.1 OFDM. This carrier does most of the work, and being high in
		// the band it degrades before the QAM channels do — so it fails first and
		// is where trouble shows up first. Watching only the QAM channels can
		// report a clean line while the OFDM carrier is losing data.
		ofdmLocked: prometheus.NewDesc(namespace+"_downstream_ofdm_locked",
			"1 if this OFDM receiver holds all three locks (PLC, NCP, MDC1) and can carry traffic. "+
				"PLC lock alone is not enough — see modemscope_downstream_ofdm_lock.",
			[]string{"receiver"}, nil),
		ofdmLockState: prometheus.NewDesc(namespace+"_downstream_ofdm_lock",
			"Per-stage OFDM lock: 1 if held. Stages: plc, ncp, mdc1. A receiver with plc=1 but "+
				"ncp=0 or mdc1=0 is only partially locked: it reports values while carrying nothing, "+
				"so its error counters freeze and rate() misreads as a clean carrier.",
			[]string{"receiver", "stage"}, nil),
		ofdmSNR: prometheus.NewDesc(namespace+"_downstream_ofdm_snr_db",
			"Downstream OFDM signal-to-noise ratio (dB).", []string{"receiver"}, nil),
		ofdmPower: prometheus.NewDesc(namespace+"_downstream_ofdm_plc_power_dbmv",
			"Downstream OFDM PLC received power (dBmV).", []string{"receiver"}, nil),
		ofdmFreq: prometheus.NewDesc(namespace+"_downstream_ofdm_subcarrier0_hz",
			"Downstream OFDM subcarrier-0 frequency (Hz).", []string{"receiver"}, nil),
		ofdmOctets: prometheus.NewDesc(namespace+"_downstream_ofdm_octets_total",
			"Downstream OFDM octets. Resets when the modem reboots.", []string{"receiver"}, nil),
		ofdmCorrected: prometheus.NewDesc(namespace+"_downstream_ofdm_correcteds_total",
			"FEC-corrected codewords on the OFDM carrier. Resets when the modem reboots.",
			[]string{"receiver"}, nil),
		ofdmUncorrect: prometheus.NewDesc(namespace+"_downstream_ofdm_uncorrectables_total",
			"Uncorrectable codewords on the OFDM carrier — unrecoverable data, i.e. real loss. "+
				"The most important error signal on a DOCSIS 3.1 line. Resets when the modem reboots.",
			[]string{"receiver"}, nil),

		usOFDMEnabled: prometheus.NewDesc(namespace+"_upstream_ofdma_enabled",
			"1 if this upstream OFDMA channel is enabled. Commonly 0 on Comcast; not a fault.",
			[]string{"channel"}, nil),
		usOFDMFreq: prometheus.NewDesc(namespace+"_upstream_ofdma_frequency_hz",
			"Upstream OFDMA centre frequency (Hz).", []string{"channel"}, nil),
		usOFDMPower: prometheus.NewDesc(namespace+"_upstream_ofdma_power_dbmv",
			"Upstream OFDMA reported transmit power (dBmV).", []string{"channel"}, nil),
		usOFDMChannelBw: prometheus.NewDesc(namespace+"_upstream_ofdma_bandwidth_hz",
			"Upstream OFDMA channel bandwidth (Hz).", []string{"channel"}, nil),

		linkUp: prometheus.NewDesc(namespace+"_lan_link_up",
			"1 if the modem's LAN port has Ethernet link. Absent if the modem does not report link status.",
			nil, nil),
		linkSpeed: prometheus.NewDesc(namespace+"_lan_link_speed_bits_per_second",
			"Negotiated speed of the modem's LAN port. A link below the service tier caps throughput.",
			nil, nil),
		linkFullDuplex: prometheus.NewDesc(namespace+"_lan_link_full_duplex",
			"1 if the LAN link negotiated full duplex. 0 on an up link usually means a bad cable or port.",
			nil, nil),
	}
}

// Describe implements prometheus.Collector.
//
// The descriptors are listed explicitly rather than via DescribeByCollect,
// which would run a full Collect — and therefore a real modem fetch — during
// registration. That would block startup on up to one budget of I/O (bad for a
// modem that reboots every few minutes), seed the cache with a startup-time
// result, and make any concurrency test that registers first meaningless,
// because registration would have already warmed the cache.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.up, c.scrapeDuration, c.info, c.uptime,
		c.dsSNR, c.dsPower, c.dsFreq, c.dsOctets, c.dsCorrected, c.dsUncorrect, c.dsChannels,
		c.usPower, c.usFreq, c.usBandwidth, c.usChannels,
		c.initState, c.networkAcces,
		c.linkUp, c.linkSpeed, c.linkFullDuplex,
		c.ofdmLocked, c.ofdmLockState, c.ofdmSNR, c.ofdmPower, c.ofdmFreq,
		c.ofdmOctets, c.ofdmCorrected, c.ofdmUncorrect,
		c.usOFDMEnabled, c.usOFDMFreq, c.usOFDMPower, c.usOFDMChannelBw,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.budget)
	defer cancel()

	start := time.Now()
	status, err := c.fetch(ctx)
	elapsed := time.Since(start).Seconds()

	ch <- prometheus.MustNewConstMetric(c.scrapeDuration, prometheus.GaugeValue, elapsed)
	if err != nil {
		// modemscope_up == 0 is itself the signal: the modem is unreachable or
		// rebooting. Emit nothing else so stale channel values can't look live.
		c.log.Warn("modem scrape failed", "err", err)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)

	ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1,
		status.Model.ModelName, status.Model.VendorName,
		status.SysInfo.HWVersion, status.SysInfo.SWVersion,
		status.SysInfo.SerialNumber, status.SysInfo.RFMac,
		status.DocsisWan.ConfigName)

	if d, uErr := ParseUptime(status.SysInfo.SystemUptime); uErr == nil {
		ch <- prometheus.MustNewConstMetric(c.uptime, prometheus.GaugeValue, d.Seconds())
	} else {
		c.log.Warn("could not parse uptime", "value", status.SysInfo.SystemUptime, "err", uErr)
	}

	ch <- prometheus.MustNewConstMetric(c.dsChannels, prometheus.GaugeValue, float64(len(status.Downstream)))
	for _, dc := range status.Downstream {
		lbl := []string{dc.ChannelID, dc.PortID}
		emit(ch, c.dsSNR, prometheus.GaugeValue, dc.SNR, lbl)
		emit(ch, c.dsPower, prometheus.GaugeValue, dc.SignalStrength, lbl)
		emit(ch, c.dsFreq, prometheus.GaugeValue, dc.Frequency, lbl)
		emit(ch, c.dsOctets, prometheus.CounterValue, dc.DSOctets, lbl)
		emit(ch, c.dsCorrected, prometheus.CounterValue, dc.Correcteds, lbl)
		emit(ch, c.dsUncorrect, prometheus.CounterValue, dc.Uncorrect, lbl)
	}

	ch <- prometheus.MustNewConstMetric(c.usChannels, prometheus.GaugeValue, float64(len(status.Upstream)))
	for _, uc := range status.Upstream {
		lbl := []string{uc.ChannelID, uc.PortID}
		emit(ch, c.usPower, prometheus.GaugeValue, uc.SignalStrength, lbl)
		emit(ch, c.usFreq, prometheus.GaugeValue, uc.Frequency, lbl)
		emit(ch, c.usBandwidth, prometheus.GaugeValue, uc.Bandwidth, lbl)
	}

	for _, oc := range status.DownstreamOFDM {
		lbl := []string{oc.Receive}
		ch <- prometheus.MustNewConstMetric(c.ofdmLocked, prometheus.GaugeValue,
			boolToFloat(oc.FullyLocked()), oc.Receive)
		for stage, held := range map[string]bool{
			"plc": yes(oc.PLCLock), "ncp": yes(oc.NCPLock), "mdc1": yes(oc.MDC1Lock),
		} {
			ch <- prometheus.MustNewConstMetric(c.ofdmLockState, prometheus.GaugeValue,
				boolToFloat(held), oc.Receive, stage)
		}
		// Gate the values on PLC lock, not full lock: PLC lock is what makes the
		// fields real rather than "NA". A partially-locked receiver's values are
		// genuine and worth exporting — the lock stages above are what reveal that
		// its frozen counters mean "carrying nothing", not "clean".
		if !oc.Locked() {
			// An unlocked receiver reports "NA" for everything else. parseFloat
			// rejects those, but skip explicitly: exporting zeros here would read
			// as a perfectly quiet channel rather than an absent one.
			continue
		}
		emit(ch, c.ofdmSNR, prometheus.GaugeValue, oc.SNR, lbl)
		emit(ch, c.ofdmPower, prometheus.GaugeValue, oc.PLCPower, lbl)
		emit(ch, c.ofdmFreq, prometheus.GaugeValue, oc.Subcarrier, lbl)
		emit(ch, c.ofdmOctets, prometheus.CounterValue, oc.DSOctets, lbl)
		emit(ch, c.ofdmCorrected, prometheus.CounterValue, oc.Correcteds, lbl)
		emit(ch, c.ofdmUncorrect, prometheus.CounterValue, oc.Uncorrect, lbl)
	}

	for _, uo := range status.UpstreamOFDM {
		lbl := []string{uo.Index}
		ch <- prometheus.MustNewConstMetric(c.usOFDMEnabled, prometheus.GaugeValue,
			boolToFloat(uo.Enabled()), uo.Index)
		if !uo.Enabled() {
			continue
		}
		emit(ch, c.usOFDMFreq, prometheus.GaugeValue, uo.Frequency, lbl)
		emit(ch, c.usOFDMPower, prometheus.GaugeValue, uo.RepPower, lbl)
		emit(ch, c.usOFDMChannelBw, prometheus.GaugeValue, uo.ChannelBw, lbl)
	}

	for stage, v := range map[string]string{
		"hw_init":         status.CMInit.HWInit,
		"find_downstream": status.CMInit.FindDownstream,
		"ranging":         status.CMInit.Ranging,
		"dhcp":            status.CMInit.DHCP,
		"time_of_day":     status.CMInit.TimeOfDay,
		"download_cfg":    status.CMInit.DownloadCfg,
		"registration":    status.CMInit.Registration,
		"bpi":             status.CMInit.BPIStatus,
		"traffic":         status.CMInit.TrafficStatus,
	} {
		ch <- prometheus.MustNewConstMetric(c.initState, prometheus.GaugeValue, boolToFloat(isSuccess(v)), stage)
	}

	ch <- prometheus.MustNewConstMetric(c.networkAcces, prometheus.GaugeValue,
		boolToFloat(isSuccess(status.CMInit.NetworkAccess)))

	// Best-effort: absent entirely when the modem didn't answer, never a fake 0.
	if l := status.Link; l != nil {
		ch <- prometheus.MustNewConstMetric(c.linkUp, prometheus.GaugeValue, boolToFloat(l.Up()))
		if bps, err := ParseLinkSpeed(l.Speed); err == nil {
			ch <- prometheus.MustNewConstMetric(c.linkSpeed, prometheus.GaugeValue, bps)
		}
		// Duplex is only meaningful while the link is up.
		if l.Up() {
			ch <- prometheus.MustNewConstMetric(c.linkFullDuplex, prometheus.GaugeValue, boolToFloat(l.FullDuplex()))
		}
	}
}

// fetch reads the modem, coalescing concurrent scrapes onto one request set.
//
// The TTL does the coalescing, not the mutex: a waiter blocks on the lock,
// acquires it after the in-flight fetch completes, re-checks the TTL, and finds
// the just-stored result. The mutex alone only serializes — with ttl=0 five
// concurrent scrapes still produce five full fetches (measured: 30 requests).
//
// So ttl must stay > 0: it is load-bearing protection for a fragile embedded
// server, not a caching nicety. Dropping it to chase fresher samples silently
// restores the dogpile and multiplies waiter latency by the scrape count.
func (c *Collector) fetch(ctx context.Context) (*Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastAt.IsZero() && time.Since(c.lastAt) < c.ttl {
		return c.lastStatus, c.lastErr
	}
	status, err := c.client.Fetch(ctx)
	c.lastStatus, c.lastErr, c.lastAt = status, err, time.Now()
	return status, err
}

// emit skips the metric when the firmware gives a non-numeric placeholder,
// rather than reporting a misleading zero.
func emit(ch chan<- prometheus.Metric, d *prometheus.Desc, t prometheus.ValueType, raw string, labels []string) {
	v, ok := parseFloat(raw)
	if !ok {
		return
	}
	ch <- prometheus.MustNewConstMetric(d, t, v, labels...)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
