package hitron

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseUptime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		// The exact string the CODA-56 emitted when it was caught 3 minutes after a reboot.
		{"observed short", "00h:02m:57s", 2*time.Minute + 57*time.Second},
		{"hours", "13h:05m:01s", 13*time.Hour + 5*time.Minute + time.Second},
		{"zero", "00h:00m:00s", 0},
		{"leading days", "3d 04h:05m:06s", 3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second},
		{"days colon", "12d:01h:02m:03s", 12*24*time.Hour + time.Hour + 2*time.Minute + 3*time.Second},
		{"spaced", " 01h:02m:03s ", time.Hour + 2*time.Minute + 3*time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseUptime(tc.in)
			if err != nil {
				t.Fatalf("ParseUptime(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseUptime(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseUptimeRejectsGarbage(t *testing.T) {
	t.Parallel()
	// Must error rather than silently return 0 — a fake uptime of 0 would look
	// like a permanent reboot and drown the reboot alert in false positives.
	for _, in := range []string{"", "TODO", "--", "up 3 minutes", "0:02:57", "02m:57s"} {
		if d, err := ParseUptime(in); err == nil {
			t.Errorf("ParseUptime(%q) = %v, want error", in, d)
		}
	}
}

func TestParseLinkSpeed(t *testing.T) {
	t.Parallel()
	ok := map[string]float64{
		"1000Mbps": 1e9, "100Mbps": 1e8, "10Mbps": 1e7, "2500Mbps": 2.5e9,
		"2.5Gbps": 2.5e9, " 1000 Mbps ": 1e9, "1gbps": 1e9,
	}
	for in, want := range ok {
		got, err := ParseLinkSpeed(in)
		if err != nil || got != want {
			t.Errorf("ParseLinkSpeed(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// Must error, not return 0 — a fake zero speed would read as a dead link.
	for _, in := range []string{"", "--", "TODO", "fast", "1000", "Mbps"} {
		if v, err := ParseLinkSpeed(in); err == nil {
			t.Errorf("ParseLinkSpeed(%q) = %v, want error", in, v)
		}
	}
}

func TestParseFloat(t *testing.T) {
	t.Parallel()
	ok := map[string]float64{"38.983": 38.983, "-1.100": -1.1, "0": 0, "46.760": 46.76}
	for in, want := range ok {
		if got, valid := parseFloat(in); !valid || got != want {
			t.Errorf("parseFloat(%q) = %v,%v want %v,true", in, got, valid, want)
		}
	}
	// Firmware placeholders must be reported as absent, not as zero.
	for _, in := range []string{"", "TODO", "--", "n/a"} {
		if _, valid := parseFloat(in); valid {
			t.Errorf("parseFloat(%q) = valid, want invalid", in)
		}
	}
}

func TestIsSuccess(t *testing.T) {
	t.Parallel()
	yes := []string{"Success", "success", "Permitted", "Enable",
		"AUTH:authorized, TEK:operational"} // real bpiStatus value
	for _, v := range yes {
		if !isSuccess(v) {
			t.Errorf("isSuccess(%q) = false, want true", v)
		}
	}
	no := []string{"Failure", "Disable", "", "AUTH:unauthorized", "Rejected"}
	for _, v := range no {
		if isSuccess(v) {
			t.Errorf("isSuccess(%q) = true, want false", v)
		}
	}
}

// fakeModem serves the exact payload shapes captured from the CODA-56.
func fakeModem(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/data/getSysInfo.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"hwVersion":"1A","swVersion":"7.3.5.3.2b1","serialNumber":"AN0000000000","rfMac":"00:11:22:33:44:55","wanIp":"TODO","systemUptime":"00h:02m:57s","systemTime":"Tue Jul 14, 2026, 20:20:28"}]`))
	})
	mux.HandleFunc("/data/dsinfo.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"portId":"1","frequency":"561000000","modulation":"2","signalStrength":"-1.100","snr":"38.983","dsoctets":"19840672","correcteds":"0","uncorrect":"0","channelId":"20"}]`))
	})
	mux.HandleFunc("/data/usinfo.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"portId":"1","frequency":"10400000","bandwidth":"3200000","modtype":"16QAM","scdmaMode":"ATDMA","signalStrength":"46.760","channelId":"1"}]`))
	})
	// Two OFDM receivers: an unused one ("NA" placeholders everywhere) and a locked
	// one carrying real values. Shapes are verbatim from the CODA-56; counter values
	// are a point-in-time sample and drift from the live device.
	mux.HandleFunc("/data/dsofdminfo.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"receive":"0","ffttype":"NA","Subcarr0freqFreq":"NA","plclock":"NO","ncplock":"NO","mdc1lock":"NO","plcpower":"NA","SNR":"NA","dsoctets":"NA","correcteds":"NA","uncorrect":"NA"},{"receive":"1","ffttype":"4K","Subcarr0freqFreq":" 713600000","plclock":"YES","ncplock":"YES","mdc1lock":"YES","plcpower":"-5.200001","SNR":"38","dsoctets":"3211241","correcteds":"3206076","uncorrect":"1432"}]`))
	})
	mux.HandleFunc("/data/usofdminfo.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"uschindex":"0","state":"  DISABLED","frequency":"0","digAtten":"    0.0000","digAttenBo":"    0.0000","channelBw":"    0.0000","repPower":"    0.0000","repPower1_6":"    0.0000","fftVal":"2K"}]`))
	})
	mux.HandleFunc("/data/getCMInit.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"hwInit":"Success","findDownstream":"Success","ranging":"Success","dhcp":"Success","timeOfday":"Success","downloadCfg":"Success","registration":"Success","eaeStatus":"Disable","bpiStatus":"AUTH:authorized, TEK:operational","networkAccess":"Permitted","trafficStatus":"Enable"}]`))
	})
	mux.HandleFunc("/data/getCmDocsisWan.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"Configname":"d11_m_coda56_subnxmgig_c01.cm","NetworkAccess":"Permitted","CmIpAddress":"2001:db8::1"}]`))
	})
	// Bare object, not an array — the one endpoint that differs.
	mux.HandleFunc("/data/system_model.asp", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"modelName" : "CODA","vendorname" : "HITRON"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch(t *testing.T) {
	t.Parallel()
	srv := fakeModem(t)
	c := NewClient(srv.URL, 5*time.Second)

	st, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if st.SysInfo.SWVersion != "7.3.5.3.2b1" {
		t.Errorf("SWVersion = %q", st.SysInfo.SWVersion)
	}
	if st.Model.ModelName != "CODA" || st.Model.VendorName != "HITRON" {
		t.Errorf("Model = %+v, want CODA/HITRON (bare-object endpoint)", st.Model)
	}
	if len(st.Downstream) != 1 || st.Downstream[0].SNR != "38.983" {
		t.Errorf("Downstream = %+v", st.Downstream)
	}
	if len(st.Upstream) != 1 || st.Upstream[0].SignalStrength != "46.760" {
		t.Errorf("Upstream = %+v", st.Upstream)
	}
	if !isSuccess(st.CMInit.Registration) || !isSuccess(st.CMInit.BPIStatus) {
		t.Errorf("CMInit = %+v", st.CMInit)
	}
	if st.DocsisWan.ConfigName != "d11_m_coda56_subnxmgig_c01.cm" {
		t.Errorf("ConfigName = %q", st.DocsisWan.ConfigName)
	}

	// OFDM is where DOCSIS 3.1 trouble shows up first, so its parsing matters most.
	if len(st.DownstreamOFDM) != 2 {
		t.Fatalf("DownstreamOFDM = %d receivers, want 2", len(st.DownstreamOFDM))
	}
	if st.DownstreamOFDM[0].Locked() {
		t.Error("receiver 0 reports locked; plclock is NO")
	}
	if !st.DownstreamOFDM[1].Locked() {
		t.Error("receiver 1 reports unlocked; plclock is YES")
	}
	if got := st.DownstreamOFDM[1].Uncorrect; got != "1432" {
		t.Errorf("OFDM uncorrect = %q, want 1432", got)
	}
	// The padded value must parse — Hitron pads these with leading spaces.
	if f, ok := parseFloat(st.DownstreamOFDM[1].Subcarrier); !ok || f != 713600000 {
		t.Errorf("OFDM subcarrier %q -> %v,%v; want 713600000,true (leading space must not break parsing)",
			st.DownstreamOFDM[1].Subcarrier, f, ok)
	}
	// An unlocked receiver's "NA" must be reported absent, never as 0 — a 0 would
	// read as a perfectly clean channel instead of a missing one.
	if _, ok := parseFloat(st.DownstreamOFDM[0].SNR); ok {
		t.Error(`unlocked receiver SNR "NA" parsed as a number; must be absent`)
	}
	if len(st.UpstreamOFDM) != 1 || st.UpstreamOFDM[0].Enabled() {
		t.Errorf("UpstreamOFDM = %+v; want one DISABLED channel (padded state must trim)", st.UpstreamOFDM)
	}
}

func TestFetchErrorsOnBadStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	if _, err := NewClient(srv.URL, 2*time.Second).Fetch(context.Background()); err == nil {
		t.Fatal("Fetch on 500 = nil error, want error (so modemscope_up goes 0)")
	}
}
