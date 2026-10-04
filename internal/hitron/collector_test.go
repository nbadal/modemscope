package hitron

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// modemServer serves the real CODA-56 payload shapes, and can be flipped to
// "rebooting" (500s) mid-test.
func modemServer(t *testing.T, healthy *atomic.Bool) *httptest.Server {
	t.Helper()
	return modemServerWith(t, healthy, nil)
}

// modemServerWith is modemServer with per-path response bodies replaced by overrides.
func modemServerWith(t *testing.T, healthy *atomic.Bool, overrides map[string]string) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/data/getSysInfo.asp":     `[{"hwVersion":"1A","swVersion":"7.3.5.3.2b1","serialNumber":"AN0000000000","rfMac":"00:11:22:33:44:55","wanIp":"TODO","systemUptime":"00h:05m:00s","systemTime":"Tue Jul 14, 2026, 20:20:28"}]`,
		"/data/dsinfo.asp":         `[{"portId":"1","channelId":"20","frequency":"561000000","modulation":"2","signalStrength":"-1.100","snr":"38.983","dsoctets":"19840672","correcteds":"3","uncorrect":"7"}]`,
		"/data/usinfo.asp":         `[{"portId":"1","channelId":"1","frequency":"10400000","bandwidth":"3200000","modtype":"16QAM","scdmaMode":"ATDMA","signalStrength":"46.760"}]`,
		"/data/getCMInit.asp":      `[{"hwInit":"Success","findDownstream":"Success","ranging":"Success","dhcp":"Success","timeOfday":"Success","downloadCfg":"Success","registration":"Success","eaeStatus":"Disable","bpiStatus":"AUTH:authorized, TEK:operational","networkAccess":"Permitted","trafficStatus":"Enable"}]`,
		"/data/getCmDocsisWan.asp": `[{"Configname":"d11_m_coda56_subnxmgig_c01.cm","NetworkAccess":"Permitted","CmIpAddress":"2001:db8::1"}]`,
		"/data/dsofdminfo.asp":     `[{"receive":"0","ffttype":"NA","Subcarr0freqFreq":"NA","plclock":"NO","ncplock":"NO","mdc1lock":"NO","plcpower":"NA","SNR":"NA","dsoctets":"NA","correcteds":"NA","uncorrect":"NA"},{"receive":"1","ffttype":"4K","Subcarr0freqFreq":" 713600000","plclock":"YES","ncplock":"YES","mdc1lock":"YES","plcpower":"-5.200001","SNR":"38","dsoctets":"3211241","correcteds":"3206076","uncorrect":"1432"}]`,
		"/data/usofdminfo.asp":     `[{"uschindex":"0","state":"  DISABLED","frequency":"0","digAtten":"    0.0000","digAttenBo":"    0.0000","channelBw":"    0.0000","repPower":"    0.0000","repPower1_6":"    0.0000","fftVal":"2K"}]`,
		"/data/system_model.asp":   `{"modelName":"CODA","vendorname":"HITRON"}`,
	}
	for path, body := range overrides {
		bodies[path] = body
	}
	mux := http.NewServeMux()
	for path, body := range bodies {
		b := body
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			if healthy != nil && !healthy.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(b))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestCollector(t *testing.T, url string) *Collector {
	t.Helper()
	return NewCollector(NewClient(url, 2*time.Second), quietLogger(), 2*time.Second)
}

func TestCollectHealthy(t *testing.T) {
	t.Parallel()
	healthy := &atomic.Bool{}
	healthy.Store(true)
	srv := modemServer(t, healthy)

	reg := prometheus.NewRegistry()
	if err := reg.Register(newTestCollector(t, srv.URL)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	expected := `
# HELP modemscope_up 1 if the modem status endpoints were scraped successfully.
# TYPE modemscope_up gauge
modemscope_up 1
# HELP modemscope_uptime_seconds Modem uptime. A drop means the modem rebooted, which also resets every error counter below.
# TYPE modemscope_uptime_seconds gauge
modemscope_uptime_seconds 300
# HELP modemscope_downstream_snr_db Downstream signal-to-noise ratio (dB). Below ~33 dB risks uncorrectable errors on 256QAM.
# TYPE modemscope_downstream_snr_db gauge
modemscope_downstream_snr_db{channel="20",port="1"} 38.983
# HELP modemscope_downstream_uncorrectables_total Uncorrectable codewords — the leading indicator of plant trouble. Resets when the modem reboots.
# TYPE modemscope_downstream_uncorrectables_total counter
modemscope_downstream_uncorrectables_total{channel="20",port="1"} 7
# HELP modemscope_upstream_power_dbmv Upstream transmit power (dBmV). Healthy range is roughly 35..51; sustained highs mean the modem is straining.
# TYPE modemscope_upstream_power_dbmv gauge
modemscope_upstream_power_dbmv{channel="1",port="1"} 46.76
# HELP modemscope_network_access 1 if the CMTS permits the modem on the network.
# TYPE modemscope_network_access gauge
modemscope_network_access 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"modemscope_up", "modemscope_uptime_seconds", "modemscope_downstream_snr_db",
		"modemscope_downstream_uncorrectables_total", "modemscope_upstream_power_dbmv",
		"modemscope_network_access"); err != nil {
		t.Error(err)
	}
}

// The OFDM path carries the diagnosis on a DOCSIS 3.1 line, so pin its exact
// values. Without this, the whole OFDM block could be deleted — or report
// correcteds in the uncorrectables series — and the suite would stay green.
func TestCollectOFDM(t *testing.T) {
	t.Parallel()
	healthy := &atomic.Bool{}
	healthy.Store(true)
	srv := modemServer(t, healthy)

	reg := prometheus.NewRegistry()
	if err := reg.Register(newTestCollector(t, srv.URL)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Fixture: receiver 0 unlocked ("NA" everywhere), receiver 1 fully locked.
	expected := `
# HELP modemscope_downstream_ofdm_uncorrectables_total Uncorrectable codewords on the OFDM carrier — unrecoverable data, i.e. real loss. The most important error signal on a DOCSIS 3.1 line. Resets when the modem reboots.
# TYPE modemscope_downstream_ofdm_uncorrectables_total counter
modemscope_downstream_ofdm_uncorrectables_total{receiver="1"} 1432
# HELP modemscope_downstream_ofdm_correcteds_total FEC-corrected codewords on the OFDM carrier. Resets when the modem reboots.
# TYPE modemscope_downstream_ofdm_correcteds_total counter
modemscope_downstream_ofdm_correcteds_total{receiver="1"} 3206076
# HELP modemscope_downstream_ofdm_snr_db Downstream OFDM signal-to-noise ratio (dB).
# TYPE modemscope_downstream_ofdm_snr_db gauge
modemscope_downstream_ofdm_snr_db{receiver="1"} 38
# HELP modemscope_downstream_ofdm_plc_power_dbmv Downstream OFDM PLC received power (dBmV).
# TYPE modemscope_downstream_ofdm_plc_power_dbmv gauge
modemscope_downstream_ofdm_plc_power_dbmv{receiver="1"} -5.200001
# HELP modemscope_downstream_ofdm_subcarrier0_hz Downstream OFDM subcarrier-0 frequency (Hz).
# TYPE modemscope_downstream_ofdm_subcarrier0_hz gauge
modemscope_downstream_ofdm_subcarrier0_hz{receiver="1"} 7.136e+08
# HELP modemscope_downstream_ofdm_locked 1 if this OFDM receiver holds all three locks (PLC, NCP, MDC1) and can carry traffic. PLC lock alone is not enough — see modemscope_downstream_ofdm_lock.
# TYPE modemscope_downstream_ofdm_locked gauge
modemscope_downstream_ofdm_locked{receiver="0"} 0
modemscope_downstream_ofdm_locked{receiver="1"} 1
# HELP modemscope_upstream_ofdma_enabled 1 if this upstream OFDMA channel is enabled. Commonly 0 on Comcast; not a fault.
# TYPE modemscope_upstream_ofdma_enabled gauge
modemscope_upstream_ofdma_enabled{channel="0"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"modemscope_downstream_ofdm_uncorrectables_total",
		"modemscope_downstream_ofdm_correcteds_total",
		"modemscope_downstream_ofdm_snr_db",
		"modemscope_downstream_ofdm_plc_power_dbmv",
		"modemscope_downstream_ofdm_subcarrier0_hz",
		"modemscope_downstream_ofdm_locked",
		"modemscope_upstream_ofdma_enabled"); err != nil {
		t.Error(err)
	}

	// The unlocked receiver's "NA" fields must be ABSENT, not 0 — a 0 SNR would
	// read as a catastrophically bad channel, a 0 counter as a perfectly clean one.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "modemscope_downstream_ofdm_") ||
			mf.GetName() == "modemscope_downstream_ofdm_locked" ||
			mf.GetName() == "modemscope_downstream_ofdm_lock" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "receiver" && l.GetValue() == "0" {
					t.Errorf("%s present for the unlocked receiver 0; NA fields must be absent", mf.GetName())
				}
			}
		}
	}

	// Disabled OFDMA must not publish a fabricated 0.0000 transmit power.
	for _, mf := range mfs {
		if mf.GetName() == "modemscope_upstream_ofdma_power_dbmv" {
			t.Error("upstream_ofdma_power_dbmv present for a DISABLED channel; must be absent")
		}
	}
}

// Partial lock is the case that would silently misreport a failing carrier as
// clean: values look real, but the counters freeze so rate() reads zero.
func TestOFDMPartialLock(t *testing.T) {
	t.Parallel()
	partial := DSOFDMChannel{PLCLock: "YES", NCPLock: "NO", MDC1Lock: "NO"}
	if !partial.Locked() {
		t.Error("Locked() = false; PLC lock means the fields are parseable")
	}
	if partial.FullyLocked() {
		t.Error("FullyLocked() = true with ncp/mdc1 NO; it cannot carry traffic")
	}
	full := DSOFDMChannel{PLCLock: "YES", NCPLock: "YES", MDC1Lock: "YES"}
	if !full.FullyLocked() {
		t.Error("FullyLocked() = false with all three locks held")
	}
}

// An unrecognized OFDMA state must not default to enabled and publish a fake 0
// transmit power, which for an upstream radio reads as dead rather than absent.
func TestOFDMAEnabledRequiresRealFrequency(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"  DISABLED", "DISABLE", "", "NA", "UNKNOWN"} {
		c := USOFDMChannel{State: state, Frequency: "0", RepPower: "    0.0000"}
		if c.Enabled() {
			t.Errorf("Enabled() = true for state %q with frequency 0", state)
		}
	}
	if !(USOFDMChannel{State: "ACTIVE", Frequency: "35600000"}).Enabled() {
		t.Error("Enabled() = false for a channel with a real frequency")
	}
}

// An enabled OFDMA channel, verbatim from a Hitron CODA-57 (sw 7.3.5.3.3b2) on
// Astound. The earlier fixtures only had a disabled channel, so the units of the
// real values were never exercised: channelBw is in MHz ("16.0000"), not Hz, and
// must be converted to match the _hz metric name; and frequency (4.6 MHz on a
// 16 MHz-wide channel) is the channel's low edge, not its centre.
func TestCollectOFDMAEnabled(t *testing.T) {
	t.Parallel()
	srv := modemServerWith(t, nil, map[string]string{
		"/data/usofdminfo.asp": `[{"uschindex":"0","state":"   OPERATE","frequency":"4600000","digAtten":"    0.2048","digAttenBo":"    8.6473","channelBw":"   16.0000","repPower":"   57.5000","repPower1_6":"   47.5000","fftVal":"2K"},{"uschindex":"1","state":"  DISABLED","frequency":"0","digAtten":"    0.0000","digAttenBo":"    0.0000","channelBw":"    0.0000","repPower":"    0.0000","repPower1_6":"    0.0000","fftVal":"2K"}]`,
	})
	reg := prometheus.NewRegistry()
	if err := reg.Register(newTestCollector(t, srv.URL)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	expected := `
# HELP modemscope_upstream_ofdma_bandwidth_hz Upstream OFDMA channel bandwidth (Hz).
# TYPE modemscope_upstream_ofdma_bandwidth_hz gauge
modemscope_upstream_ofdma_bandwidth_hz{channel="0"} 1.6e+07
# HELP modemscope_upstream_ofdma_frequency_hz Upstream OFDMA start frequency (Hz): the low edge of the channel, not its centre.
# TYPE modemscope_upstream_ofdma_frequency_hz gauge
modemscope_upstream_ofdma_frequency_hz{channel="0"} 4.6e+06
# HELP modemscope_upstream_ofdma_enabled 1 if this upstream OFDMA channel is enabled. Commonly 0 on Comcast; not a fault.
# TYPE modemscope_upstream_ofdma_enabled gauge
modemscope_upstream_ofdma_enabled{channel="0"} 1
modemscope_upstream_ofdma_enabled{channel="1"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"modemscope_upstream_ofdma_bandwidth_hz", "modemscope_upstream_ofdma_frequency_hz",
		"modemscope_upstream_ofdma_enabled"); err != nil {
		t.Error(err)
	}
}

// Error counters must be counters, not gauges: they reset on every modem reboot,
// and only a counter lets rate()/increase() handle the reset correctly.
func TestErrorCountersAreCounters(t *testing.T) {
	t.Parallel()
	healthy := &atomic.Bool{}
	healthy.Store(true)
	srv := modemServer(t, healthy)

	reg := prometheus.NewRegistry()
	if err := reg.Register(newTestCollector(t, srv.URL)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := map[string]string{
		"modemscope_downstream_uncorrectables_total": "COUNTER",
		"modemscope_downstream_correcteds_total":     "COUNTER",
		"modemscope_downstream_octets_total":         "COUNTER",
		"modemscope_uptime_seconds":                  "GAUGE",
		"modemscope_downstream_snr_db":               "GAUGE",
		// The OFDM counters matter most on a 3.1 line; as gauges rate() would
		// break across the modem's frequent reboots.
		"modemscope_downstream_ofdm_uncorrectables_total": "COUNTER",
		"modemscope_downstream_ofdm_correcteds_total":     "COUNTER",
		"modemscope_downstream_ofdm_octets_total":         "COUNTER",
		"modemscope_downstream_ofdm_snr_db":               "GAUGE",
		"modemscope_downstream_ofdm_locked":               "GAUGE",
	}
	seen := map[string]string{}
	for _, mf := range mfs {
		seen[mf.GetName()] = mf.GetType().String()
	}
	for name, typ := range want {
		if got, ok := seen[name]; !ok {
			t.Errorf("%s missing", name)
		} else if got != typ {
			t.Errorf("%s is %s, want %s", name, got, typ)
		}
	}
}

// On a failed scrape the collector must emit up=0 and nothing else — reporting
// stale channel values as if fresh would misrepresent a dead modem as healthy.
func TestCollectUnreachableEmitsOnlyUp(t *testing.T) {
	t.Parallel()
	healthy := &atomic.Bool{} // false: modem rebooting
	srv := modemServer(t, healthy)

	reg := prometheus.NewRegistry()
	if err := reg.Register(newTestCollector(t, srv.URL)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := testutil.ToFloat64(mustGauge(t, reg, "modemscope_up")); got != 0 {
		t.Errorf("modemscope_up = %v, want 0", got)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		switch mf.GetName() {
		case "modemscope_downstream_snr_db", "modemscope_uptime_seconds",
			"modemscope_downstream_uncorrectables_total", "modemscope_info":
			t.Errorf("%s present while modem unreachable; must be absent", mf.GetName())
		}
	}
}

// The exporter is expected to start while the modem is mid-reboot (this modem
// reboots often). Registration must not freeze the metric set to the degraded
// one — every metric has to appear once the modem returns.
//
// Uses a pedantic registry deliberately: a plain one never checks collected
// metrics against the described set, so this would pass no matter what Describe
// emitted. Pedantic mode enforces that Describe covers everything Collect can
// produce, which is the property actually worth guarding.
func TestRegisterWhileDownThenRecover(t *testing.T) {
	t.Parallel()
	healthy := &atomic.Bool{} // starts down
	srv := modemServer(t, healthy)

	c := newTestCollector(t, srv.URL)
	// Disable result coalescing: this test asserts the metric set isn't frozen by
	// a failed registration, which is independent of caching. With the default
	// TTL the recovery Gather would replay a cached failure purely because the
	// test runs faster than the TTL (in production the 30s scrape interval is far
	// wider).
	c.ttl = 0

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register while modem down: %v", err)
	}

	healthy.Store(true)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather after recovery: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "modemscope_downstream_snr_db" {
			found = true
		}
	}
	if !found {
		t.Error("downstream metrics missing after the modem recovered")
	}
}

// Concurrent scrapes must coalesce onto one set of modem requests: the modem's
// embedded server is fragile, and a dogpile is exactly what breaks it.
func TestConcurrentScrapesCoalesce(t *testing.T) {
	t.Parallel()
	var hits int64
	mux := http.NewServeMux()
	bodies := map[string]string{
		"/data/getSysInfo.asp":     `[{"hwVersion":"1A","swVersion":"7.3","serialNumber":"S","rfMac":"M","systemUptime":"00h:05m:00s","systemTime":"x"}]`,
		"/data/dsinfo.asp":         `[{"portId":"1","channelId":"20","frequency":"561000000","modulation":"2","signalStrength":"-1.1","snr":"38.9","dsoctets":"1","correcteds":"0","uncorrect":"0"}]`,
		"/data/usinfo.asp":         `[{"portId":"1","channelId":"1","frequency":"1","bandwidth":"1","modtype":"16QAM","scdmaMode":"ATDMA","signalStrength":"46.7"}]`,
		"/data/getCMInit.asp":      `[{"hwInit":"Success","findDownstream":"Success","ranging":"Success","dhcp":"Success","timeOfday":"Success","downloadCfg":"Success","registration":"Success","eaeStatus":"Disable","bpiStatus":"AUTH:authorized, TEK:operational","networkAccess":"Permitted","trafficStatus":"Enable"}]`,
		"/data/getCmDocsisWan.asp": `[{"Configname":"c","NetworkAccess":"Permitted","CmIpAddress":"::1"}]`,
		"/data/dsofdminfo.asp":     `[{"receive":"0","ffttype":"NA","Subcarr0freqFreq":"NA","plclock":"NO","ncplock":"NO","mdc1lock":"NO","plcpower":"NA","SNR":"NA","dsoctets":"NA","correcteds":"NA","uncorrect":"NA"},{"receive":"1","ffttype":"4K","Subcarr0freqFreq":" 713600000","plclock":"YES","ncplock":"YES","mdc1lock":"YES","plcpower":"-5.200001","SNR":"38","dsoctets":"3211241","correcteds":"3206076","uncorrect":"1432"}]`,
		"/data/usofdminfo.asp":     `[{"uschindex":"0","state":"  DISABLED","frequency":"0","digAtten":"    0.0000","digAttenBo":"    0.0000","channelBw":"    0.0000","repPower":"    0.0000","repPower1_6":"    0.0000","fftVal":"2K"}]`,
		"/data/system_model.asp":   `{"modelName":"CODA","vendorname":"HITRON"}`,
	}
	for path, body := range bodies {
		b := body
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			time.Sleep(20 * time.Millisecond) // make overlap real
			_, _ = w.Write([]byte(b))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Must hit a COLD cache: registering first would warm it via Describe and the
	// concurrent scrapes would all be cache hits, making this assert nothing (the
	// mutex could be deleted and it would still pass).
	c := NewCollector(NewClient(srv.URL, 3*time.Second), quietLogger(), 3*time.Second)

	const scrapes = 5
	var wg sync.WaitGroup
	for range scrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan prometheus.Metric, 128)
			c.Collect(ch)
		}()
	}
	wg.Wait()

	// Exactly one fetch's worth: one request per endpoint. Uncoalesced this
	// would be scrapes * endpoints. Derived from the fixture so adding an
	// endpoint doesn't silently weaken the assertion.
	wantHits := int64(len(bodies))
	if got := atomic.LoadInt64(&hits); got != wantHits {
		t.Errorf("%d concurrent cold-cache scrapes made %d modem requests, want exactly %d (one coalesced fetch); uncoalesced would be %d",
			scrapes, got, wantHits, int64(scrapes)*wantHits)
	}
}

func mustGauge(t *testing.T, reg *prometheus.Registry, name string) prometheus.Collector {
	t.Helper()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "shim"})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			g.Set(mf.GetMetric()[0].GetGauge().GetValue())
			return g
		}
	}
	t.Fatalf("%s not found", name)
	return nil
}
