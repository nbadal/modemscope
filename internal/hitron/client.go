// Package hitron reads DOCSIS status from a Hitron cable modem's unauthenticated
// JSON endpoints (verified against a CODA-56, sw 7.3.5.3.2b1).
package hitron

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client talks to a Hitron modem's /data/*.asp endpoints.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client for baseURL (e.g. "https://192.168.100.1").
//
// The modem presents a CableLabs-issued cert whose CN is its MAC address, so it
// never validates against a normal chain — TLS verification is intentionally
// skipped. The endpoints are unauthenticated and read-only, and the modem sits
// on a link-local management subnet, so this is a status read, not a trust
// boundary we can meaningfully enforce.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see doc comment
			},
		},
	}
}

// SysInfo is /data/getSysInfo.asp.
type SysInfo struct {
	HWVersion    string `json:"hwVersion"`
	SWVersion    string `json:"swVersion"`
	SerialNumber string `json:"serialNumber"`
	RFMac        string `json:"rfMac"`
	SystemUptime string `json:"systemUptime"`
	SystemTime   string `json:"systemTime"`
}

// Model is /data/system_model.asp. Unlike the others this endpoint returns a
// bare object rather than a single-element array.
type Model struct {
	ModelName  string `json:"modelName"`
	VendorName string `json:"vendorname"`
}

// DSChannel is one downstream channel from /data/dsinfo.asp.
type DSChannel struct {
	PortID         string `json:"portId"`
	ChannelID      string `json:"channelId"`
	Frequency      string `json:"frequency"`
	Modulation     string `json:"modulation"`
	SignalStrength string `json:"signalStrength"`
	SNR            string `json:"snr"`
	DSOctets       string `json:"dsoctets"`
	Correcteds     string `json:"correcteds"`
	Uncorrect      string `json:"uncorrect"`
}

// USChannel is one upstream channel from /data/usinfo.asp.
type USChannel struct {
	PortID         string `json:"portId"`
	ChannelID      string `json:"channelId"`
	Frequency      string `json:"frequency"`
	Bandwidth      string `json:"bandwidth"`
	ModType        string `json:"modtype"`
	SCDMAMode      string `json:"scdmaMode"`
	SignalStrength string `json:"signalStrength"`
}

// DSOFDMChannel is one downstream OFDM receiver from /data/dsofdminfo.asp.
//
// On DOCSIS 3.1 the OFDM carrier does most of the work, so its error counters
// matter more than the legacy QAM channels'. Unused receivers report "NO" locks
// and "NA" everywhere; only locked receivers carry real numbers.
type DSOFDMChannel struct {
	Receive    string `json:"receive"`
	FFTType    string `json:"ffttype"`
	Subcarrier string `json:"Subcarr0freqFreq"`
	PLCLock    string `json:"plclock"`
	NCPLock    string `json:"ncplock"`
	MDC1Lock   string `json:"mdc1lock"`
	PLCPower   string `json:"plcpower"`
	SNR        string `json:"SNR"`
	DSOctets   string `json:"dsoctets"`
	Correcteds string `json:"correcteds"`
	Uncorrect  string `json:"uncorrect"`
}

// Locked reports whether this OFDM receiver has PLC lock, which is what decides
// whether its other fields hold real values or "NA" placeholders.
//
// PLC lock alone does NOT mean the receiver is decoding traffic: PLC lock comes
// first, and NCP/MDC1 lock can still be absent. Such a receiver reports values
// while carrying nothing, so its frozen counters would make rate() read zero —
// "the OFDM carrier is clean" — during precisely the marginal condition this
// exporter exists to catch. Use FullyLocked to judge health; use Locked only to
// decide whether the fields are parseable.
func (c DSOFDMChannel) Locked() bool {
	return yes(c.PLCLock)
}

// FullyLocked reports whether all three locks are held, i.e. the receiver is
// actually able to carry traffic.
func (c DSOFDMChannel) FullyLocked() bool {
	return yes(c.PLCLock) && yes(c.NCPLock) && yes(c.MDC1Lock)
}

func yes(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "yes") }

// USOFDMChannel is one upstream OFDMA channel from /data/usofdminfo.asp.
type USOFDMChannel struct {
	Index     string `json:"uschindex"`
	State     string `json:"state"`
	Frequency string `json:"frequency"`
	DigAtten  string `json:"digAtten"`
	ChannelBw string `json:"channelBw"` // MHz, unlike USChannel.Bandwidth (Hz)
	RepPower  string `json:"repPower"`
	FFTVal    string `json:"fftVal"`
}

// Enabled reports whether this OFDMA channel is in use. Comcast commonly leaves
// upstream OFDMA disabled, which is not a fault.
//
// Keyed on a real frequency rather than the state string. A disabled channel
// reports frequency "0" alongside "0.0000" power, and no enabled-state string
// has been observed on this firmware — so trusting the string would default any
// unrecognized value to "enabled" and publish that 0.0000 as a genuine transmit
// power, which for an upstream radio reads as dead rather than absent.
func (c USOFDMChannel) Enabled() bool {
	if strings.EqualFold(strings.TrimSpace(c.State), "disabled") {
		return false
	}
	f, ok := parseFloat(c.Frequency)
	return ok && f > 0
}

// CMInit is /data/getCMInit.asp — the DOCSIS registration state machine.
type CMInit struct {
	HWInit         string `json:"hwInit"`
	FindDownstream string `json:"findDownstream"`
	Ranging        string `json:"ranging"`
	DHCP           string `json:"dhcp"`
	TimeOfDay      string `json:"timeOfday"`
	DownloadCfg    string `json:"downloadCfg"`
	Registration   string `json:"registration"`
	EAEStatus      string `json:"eaeStatus"`
	BPIStatus      string `json:"bpiStatus"`
	NetworkAccess  string `json:"networkAccess"`
	TrafficStatus  string `json:"trafficStatus"`
}

// DocsisWan is /data/getCmDocsisWan.asp.
type DocsisWan struct {
	ConfigName    string `json:"Configname"`
	NetworkAccess string `json:"NetworkAccess"`
	CmIPAddress   string `json:"CmIpAddress"`
}

// Status is a full snapshot of the modem.
type Status struct {
	SysInfo        SysInfo
	Model          Model
	Downstream     []DSChannel
	Upstream       []USChannel
	DownstreamOFDM []DSOFDMChannel
	UpstreamOFDM   []USOFDMChannel
	CMInit         CMInit
	DocsisWan      DocsisWan

	// OFDMErr records a failure to read the OFDM endpoints. It does not fail the
	// scrape (see Fetch); callers surface it as absent OFDM series.
	OFDMErr error
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: unexpected status %d", path, resp.StatusCode)
	}
	// Cap the read: a wedged modem can stream garbage.
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// getOne decodes an endpoint that wraps a single object in an array.
func getOne[T any](ctx context.Context, c *Client, path string) (T, error) {
	var zero T
	body, err := c.get(ctx, path)
	if err != nil {
		return zero, err
	}
	var arr []T
	if err := json.Unmarshal(body, &arr); err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}
	if len(arr) == 0 {
		return zero, fmt.Errorf("%s: empty array", path)
	}
	return arr[0], nil
}

func getSlice[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var arr []T
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return arr, nil
}

// Fetch retrieves a full status snapshot. Endpoints are read sequentially: the
// modem runs a small embedded web server and does not appreciate concurrency.
func (c *Client) Fetch(ctx context.Context) (*Status, error) {
	var s Status
	var err error

	if s.SysInfo, err = getOne[SysInfo](ctx, c, "/data/getSysInfo.asp"); err != nil {
		return nil, err
	}
	if s.Downstream, err = getSlice[DSChannel](ctx, c, "/data/dsinfo.asp"); err != nil {
		return nil, err
	}
	if s.Upstream, err = getSlice[USChannel](ctx, c, "/data/usinfo.asp"); err != nil {
		return nil, err
	}
	// The OFDM endpoints are tolerated rather than required. They are the newest
	// and least-portable part of the surface, and making them hard dependencies
	// would mean a firmware variation on one of them blinds the whole instrument
	// — uptime, QAM channels and registration state would all vanish behind
	// up=0. A missing slice costs only the OFDM series.
	if s.DownstreamOFDM, err = getSlice[DSOFDMChannel](ctx, c, "/data/dsofdminfo.asp"); err != nil {
		s.DownstreamOFDM, s.OFDMErr = nil, err
	}
	if s.UpstreamOFDM, err = getSlice[USOFDMChannel](ctx, c, "/data/usofdminfo.asp"); err != nil {
		s.UpstreamOFDM = nil
		if s.OFDMErr == nil {
			s.OFDMErr = err
		}
	}
	if s.CMInit, err = getOne[CMInit](ctx, c, "/data/getCMInit.asp"); err != nil {
		return nil, err
	}
	if s.DocsisWan, err = getOne[DocsisWan](ctx, c, "/data/getCmDocsisWan.asp"); err != nil {
		return nil, err
	}

	// system_model.asp returns a bare object, not an array.
	if body, mErr := c.get(ctx, "/data/system_model.asp"); mErr == nil {
		_ = json.Unmarshal(body, &s.Model)
	}

	return &s, nil
}

// uptimeRe matches the CODA-56's "00h:02m:57s". The day field is speculative:
// this modem was only ever observed under 24h of uptime, so accept an optional
// leading "<n>d" in the separator styles Hitron firmware is known to mix.
var uptimeRe = regexp.MustCompile(`(?i)^\s*(?:(\d+)\s*d(?:ays?)?[\s:]*)?(\d+)\s*h[\s:]*(\d+)\s*m[\s:]*(\d+)\s*s\s*$`)

// ParseUptime converts a Hitron uptime string to seconds.
//
// This is the highest-value signal the modem exposes: every error counter it
// reports resets on reboot, so a snapshot of "0 uncorrectables" is meaningless
// unless you know the modem hasn't just restarted. An unparseable value returns
// an error rather than a wrong number — callers omit the metric instead, so a
// firmware format change surfaces as absence rather than a fake uptime.
func ParseUptime(s string) (time.Duration, error) {
	m := uptimeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("unrecognized uptime format %q", s)
	}
	atoi := func(v string) int {
		n, _ := strconv.Atoi(v)
		return n
	}
	d := time.Duration(atoi(m[1]))*24*time.Hour +
		time.Duration(atoi(m[2]))*time.Hour +
		time.Duration(atoi(m[3]))*time.Minute +
		time.Duration(atoi(m[4]))*time.Second
	return d, nil
}

// parseFloat is tolerant of the empty/placeholder values the firmware emits.
func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "TODO" || s == "--" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	// A single NaN/Inf sample poisons rate() and sum() for the whole series, so
	// treat them as placeholders too — absence is recoverable, poison is not.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// isSuccess reports whether a DOCSIS init stage reads as healthy.
func isSuccess(v string) bool {
	lower := strings.ToLower(strings.TrimSpace(v))
	switch lower {
	case "success", "permitted", "enable", "enabled", "operational":
		return true
	}
	// bpiStatus looks like "AUTH:authorized, TEK:operational". Match the
	// qualified form: a bare "authorized" check also matches "unauthorized",
	// which would report a failed BPI auth as healthy.
	return strings.Contains(lower, "auth:authorized")
}
