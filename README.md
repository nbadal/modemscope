<!-- readme-type: exporter -->
# modemscope

Prometheus exporter for Hitron DOCSIS cable modems — SNR, power, FEC errors, uptime, and registration state

A cable modem's status pages carry the cable plant's vital signs — per-channel
SNR, transmit/receive power, FEC error counters, and the DOCSIS registration
state — but none of it is retained: it's a live view that resets on every
reboot, so an intermittent line problem leaves no evidence by the time you
look. modemscope polls a Hitron modem every scrape and hands Prometheus the
history instead, so `rate()` over the error counters actually means something.

**Status:** in daily use on the homelab since 2026-07 (currently pinned to
`2026-07-26-e2a59cd`); hardware-confirmed
against a Hitron CODA-56 (DOCSIS 3.1, sw `7.3.5.3.2b1`) on Comcast.

```text
$ MODEMSCOPE_TIMEOUT=1s ./modemscope-agent &
time=2026-09-30T06:15:05.404Z level=INFO msg="metrics server listening" addr=:9104 modem=https://192.168.100.1
time=2026-09-30T06:15:07.419Z level=WARN msg="modem scrape failed" err="Get \"https://192.168.100.1/data/getSysInfo.asp\": context deadline exceeded"

$ curl -s -o /dev/null -w 'HTTP %{http_code}\n' localhost:9104/healthz
HTTP 200

$ curl -s localhost:9104/metrics | grep ^modemscope_
modemscope_scrape_duration_seconds 1.000615345
modemscope_up 0
```

## Why

Every error counter the modem exposes resets on reboot, so
`modemscope_uptime_seconds` is what makes the rest of them interpretable: "0
uncorrectables" means nothing on its own — it could be a clean line, or a
modem that restarted a minute ago, and a silently rebooting modem otherwise
looks the same as a healthy one to ping and to a speed test. Uptime is the
signal that tells the two apart, and the per-channel SNR/power/FEC metrics
tell you which channel is degrading before it takes the line down.

## Metrics

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `modemscope_up` | gauge | — | 1 if the scrape succeeded. 0 = modem unreachable or rebooting. |
| `modemscope_uptime_seconds` | gauge | — | Modem uptime. A drop = a reboot (and a counter reset). |
| `modemscope_info` | gauge | `model,vendor,hw_version,sw_version,serial,rf_mac,config_name` | Identity. Always 1. |
| `modemscope_downstream_snr_db` | gauge | `channel,port` | Downstream SNR. <~33 dB risks uncorrectables on 256QAM. |
| `modemscope_downstream_power_dbmv` | gauge | `channel,port` | Downstream power. Healthy ≈ −7..+7 dBmV. |
| `modemscope_downstream_uncorrectables_total` | counter | `channel,port` | Uncorrectable codewords — the leading indicator of plant trouble. |
| `modemscope_downstream_correcteds_total` | counter | `channel,port` | FEC-corrected codewords. |
| `modemscope_downstream_octets_total` | counter | `channel,port` | Downstream octets. |
| `modemscope_downstream_frequency_hz` | gauge | `channel,port` | Channel centre frequency. |
| `modemscope_downstream_channels` | gauge | — | Bonded downstream channel count. A drop = channels fell off. |
| `modemscope_downstream_modulation_info` | gauge | `channel,port,modulation` | Always 1; the modulation is the label (the modem's numeric code is translated: `0` 16QAM, `1` 64QAM, `2` 256QAM, `3` 1024QAM, `4` 32QAM, `5` 128QAM, `6` QPSK; anything else is shown raw). A channel stepping down to a lower modulation is an early sign of trouble. |
| `modemscope_upstream_power_dbmv` | gauge | `channel,port` | Upstream transmit power. Healthy ≈ 35..51 dBmV. |
| `modemscope_upstream_frequency_hz` / `..._bandwidth_hz` | gauge | `channel,port` | Upstream channel shape. |
| `modemscope_upstream_channels` | gauge | — | Bonded upstream channel count. |
| `modemscope_upstream_modulation_info` | gauge | `channel,port,modulation,mode` | Always 1; modulation (e.g. `64QAM`) and DOCSIS mode (e.g. `ATDMA`) as labels. |
| `modemscope_downstream_ofdm_locked` | gauge | `receiver` | 1 if the OFDM receiver holds all three locks (PLC, NCP, MDC1). |
| `modemscope_downstream_ofdm_snr_db` | gauge | `receiver` | DOCSIS 3.1 OFDM SNR. |
| `modemscope_downstream_ofdm_plc_power_dbmv` | gauge | `receiver` | OFDM PLC received power. |
| `modemscope_downstream_ofdm_uncorrectables_total` | counter | `receiver` | Uncorrectables on the OFDM carrier — the single most important error signal on a 3.1 line. |
| `modemscope_downstream_ofdm_correcteds_total` | counter | `receiver` | FEC-corrected codewords on the OFDM carrier. |
| `modemscope_downstream_ofdm_octets_total` | counter | `receiver` | OFDM octets. |
| `modemscope_downstream_ofdm_subcarrier0_hz` | gauge | `receiver` | OFDM subcarrier-0 frequency. |
| `modemscope_upstream_ofdma_enabled` | gauge | `channel` | 1 if upstream OFDMA is enabled (commonly 0 on Comcast; not a fault). |
| `modemscope_upstream_ofdma_frequency_hz` / `..._power_dbmv` / `..._bandwidth_hz` | gauge | `channel` | Upstream OFDMA shape; absent when the channel is disabled. |
| `modemscope_downstream_ofdm_lock` | gauge | `receiver,stage` | Per-stage OFDM lock (`plc`, `ncp`, `mdc1`). `plc=1` with `ncp=0`/`mdc1=0` is a partial lock: values look real but counters freeze, so `rate()` misreads it as a clean carrier. |
| `modemscope_docsis_init_state` | gauge | `stage` | 1 = healthy. Stages: `hw_init`, `find_downstream`, `ranging`, `dhcp`, `time_of_day`, `download_cfg`, `registration`, `bpi`, `traffic`. |
| `modemscope_network_access` | gauge | — | 1 if the CMTS permits the modem on the network. |
| `modemscope_scrape_duration_seconds` | gauge | — | Scrape latency (~0.2s typical). |

The counters **reset on reboot**, so always use `rate()` / `increase()` —
never compare raw totals across a restart. The query that answers the Why —
uncorrectables appearing anywhere, QAM or OFDM, since the OFDM carrier does
most of the work on a DOCSIS 3.1 line and degrades first:

```promql
sum(rate(modemscope_downstream_uncorrectables_total[15m]))
  + sum(rate(modemscope_downstream_ofdm_uncorrectables_total[15m])) > 0
```

## Quick start

Needs Go 1.23 and a Hitron cable modem reachable at `MODEMSCOPE_MODEM_URL`
(default `https://192.168.100.1`).

```bash
git clone https://github.com/gjcourt/modemscope && cd modemscope
go run ./cmd/agent
curl -s localhost:9104/metrics | grep ^modemscope_
```

## Configuration

| Env | Default | Meaning |
| --- | --- | --- |
| `MODEMSCOPE_LISTEN_ADDR` | `:9104` | metrics listen address |
| `MODEMSCOPE_MODEM_URL` | `https://192.168.100.1` | modem base URL |
| `MODEMSCOPE_TIMEOUT` | `8s` | total modem-I/O budget per scrape. Keep below Prometheus's scrape timeout, or a slow modem makes Prometheus give up before `modemscope_up=0` is delivered. |

`/metrics` exposes the collector; `/healthz` reports only that the exporter is
running — deliberately **not** whether the modem is reachable, since an
unreachable modem is a metric worth alerting on, not a reason to restart the
pod.

## How it works

modemscope polls the modem's unauthenticated GoAhead status endpoints
sequentially each scrape (TLS verification skipped — the modem's cert is
issued to its own MAC, which doesn't validate against a normal chain),
coalesces concurrent scrapes so Prometheus can't overrun the modem's small
embedded server, and translates the vendor's ad hoc JSON into the typed
metrics above; see [`ARCHITECTURE.md`](ARCHITECTURE.md) for the endpoint list,
the scrape flow, and the modem-specific quirks it works around.

## Development

Go 1.23.

```sh
go test ./...
go vet ./...
gofmt -l .
make build   # docker buildx build ... -t ghcr.io/gjcourt/modemscope:dev
make tidy    # go mod tidy
```

CI (`.github/workflows/build.yml`) runs gofmt, `go vet`, `go test`, and a
`go mod tidy` diff check on every pull request and on push to `main`. See
[`AGENTS.md`](AGENTS.md) for repo conventions.

## Deployment

CI builds and pushes the image to `ghcr.io/gjcourt/modemscope` on every push
to `main`, tagged additively as `main`, `<sha7>`, `YYYY-MM-DD`, the immutable
`YYYY-MM-DD-<sha7>`, and `latest`. In the homelab it runs as a single-replica
Deployment pinned by tag and digest in
[`apps/base/modemscope/deployment.yaml`](https://github.com/gjcourt/homelab/blob/master/apps/base/modemscope/deployment.yaml)
and rolled out via GitOps — bump the pin there to deploy a new build; don't
repoint `latest`.

## License

[Apache-2.0](LICENSE)
