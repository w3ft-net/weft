package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// MetricsConfig is the top-level `metrics:` block. Having the block at all
// turns host metrics on; the rows are written under the host's deployment.
type MetricsConfig struct {
	Interval   string   `yaml:"interval"`   // default "30s"
	Collectors []string `yaml:"collectors"` // default: all
}

// cpuState holds the previous /proc/stat sample for delta computation.
type cpuState struct {
	user, nice, system, idle, iowait, irq, softirq, steal int64
}

func (c cpuState) total() int64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

// busy is the jiffies spent doing anything but idling or waiting on IO.
func (c cpuState) busy() int64 {
	return c.user + c.nice + c.system + c.irq + c.softirq + c.steal
}

// userHZ is the /proc/stat tick rate. It is 100 on every Linux weftd runs on.
const userHZ = 100

// netState holds per-interface rx/tx bytes from the previous sample.
type netState struct {
	rxBytes, txBytes int64
}

// The kinds host metrics are written as: a pseudofile each at
// /log/<namespace>/<deployment>/<kind>.log. The agent builds the rows, so
// none of them goes through schema sampling.
const (
	kindHost    = "host"    // one row per tick: cpu, memory and load as objects
	kindDisk    = "disk"    // one row per /dev/* mount
	kindNetwork = "network" // one row per non-loopback interface
)

// effectiveDeployment is the host's deployment as its rows carry it: a
// `deployment` metavariable if one is set and has a value, else the
// `deployment:` key.
func effectiveDeployment() string {
	if v := metavarValue("", "deployment"); v != "" {
		return v
	}
	return globalDeployment
}

// runMetricsCollector runs in its own goroutine, collecting system metrics
// at the configured interval and sending them as LogRows through the
// coordinator IngestStream. Metrics describe the host, not a source, so
// they go to one place, under the host's deployment (its ASG, k8s
// Deployment, ...) as the service. The deployment is read every tick: it
// can come from a file that appears after startup.
func runMetricsCollector(ctx context.Context, client pb.WeftCoordinatorClient,
	instance, namespace string, mcfg MetricsConfig) {

	interval := 30 * time.Second
	if mcfg.Interval != "" {
		d, err := time.ParseDuration(mcfg.Interval)
		if err != nil {
			weftsdk.Infof(ctx, "metrics: bad interval %q, using 30s", mcfg.Interval)
		} else {
			interval = d
		}
	}

	collectors := map[string]bool{
		"cpu": true, "memory": true, "disk": true, "load": true, "network": true,
	}
	if len(mcfg.Collectors) > 0 {
		collectors = map[string]bool{}
		for _, c := range mcfg.Collectors {
			collectors[c] = true
		}
	}

	weftsdk.Infof(ctx, "metrics: interval=%s collectors=%v", interval, mcfg.Collectors)
	if effectiveDeployment() == "" {
		weftsdk.Errorf(ctx, "metrics: no deployment is set, so there is nowhere to write host metrics; set `deployment:` (or a deployment metavariable) and they start on their own")
	}

	ss := &streamState{}

	var prevCPU *cpuState
	prevNet := map[string]netState{}
	prevTime := time.Time{}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service := sanitizeDeployment(effectiveDeployment())
			if service == "" {
				continue
			}
			now := time.Now()
			elapsed := now.Sub(prevTime).Seconds()
			if prevTime.IsZero() {
				elapsed = 0
			}

			// What each kind sends this tick.
			sections := map[string]any{}
			var disks, nets []map[string]any

			if collectors["cpu"] {
				cur, err := readCPU()
				if err == nil {
					if prevCPU != nil {
						sections["cpu"] = cpuSection(prevCPU, cur)
					}
					prevCPU = cur
				}
			}

			if collectors["memory"] {
				if m, err := memorySection(); err == nil {
					sections["memory"] = m
				}
			}

			if collectors["disk"] {
				if rows, err := diskRows(); err == nil {
					disks = rows
				}
			}

			if collectors["load"] {
				if l, err := loadSection(); err == nil {
					sections["load"] = l
				}
			}

			if collectors["network"] {
				curNet, err := readNetDev()
				if err == nil && len(prevNet) > 0 && elapsed > 0 {
					nets = networkRows(prevNet, curNet, elapsed)
				}
				if err == nil {
					prevNet = curNet
				}
			}

			prevTime = now

			var rows []*pb.LogRow
			if len(sections) > 0 {
				rows = append(rows, metricRow(kindHost, instance, now, sections))
			}
			for _, d := range disks {
				rows = append(rows, metricRow(kindDisk, instance, now, d))
			}
			for _, n := range nets {
				rows = append(rows, metricRow(kindNetwork, instance, now, n))
			}

			for _, row := range rows {
				row.Namespace, row.Service = namespace, service
				if err := sendRowWithReconnect(ctx, client, ss, row, namespace+"/"+service+"/"+row.Kind); err != nil {
					return // context cancelled
				}
			}
		}
	}
}

// metricRow is one LogRow of kind whose JSON payload is fields. The payload
// carries nothing the row's columns already hold: the time, host and
// deployment are its ts, instance and deployment, which queries read as $ts,
// $host and $deployment.
func metricRow(kind, instance string, now time.Time, fields map[string]any) *pb.LogRow {
	body, _ := json.Marshal(fields)
	return &pb.LogRow{
		Instance: instance, Kind: kind,
		Raw: string(body), Level: "INFO", TsUnixNs: now.UnixNano(),
		PayloadJson: body,
	}
}

// ── CPU ──────────────────────────────────────────────────────────────────────

func readCPU() (*cpuState, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			return nil, fmt.Errorf("unexpected /proc/stat format")
		}
		s := &cpuState{}
		s.user, _ = strconv.ParseInt(fields[1], 10, 64)
		s.nice, _ = strconv.ParseInt(fields[2], 10, 64)
		s.system, _ = strconv.ParseInt(fields[3], 10, 64)
		s.idle, _ = strconv.ParseInt(fields[4], 10, 64)
		s.iowait, _ = strconv.ParseInt(fields[5], 10, 64)
		s.irq, _ = strconv.ParseInt(fields[6], 10, 64)
		s.softirq, _ = strconv.ParseInt(fields[7], 10, 64)
		s.steal, _ = strconv.ParseInt(fields[8], 10, 64)
		return s, nil
	}
	return nil, fmt.Errorf("/proc/stat: no cpu line")
}

// cpuSection is the "cpu" object: percentages of the time between two
// /proc/stat samples, plus time_s, the cumulative busy CPU seconds since boot
// (a counter, for consumers that compute their own rates).
func cpuSection(prev, cur *cpuState) map[string]any {
	dt := float64(cur.total() - prev.total())
	if dt == 0 {
		dt = 1
	}
	return map[string]any{
		"user":   round2(float64(cur.user-prev.user) / dt * 100),
		"system": round2(float64(cur.system-prev.system) / dt * 100),
		"idle":   round2(float64(cur.idle-prev.idle) / dt * 100),
		"iowait": round2(float64(cur.iowait-prev.iowait) / dt * 100),
		"time_s": float64(cur.busy()) / userHZ,
		// Logical CPUs, counted from /proc/stat (cpu0, cpu1, ...).
		"cores": countCPUs(),
	}
}

func countCPUs() int {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 1
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}

// ── Memory ───────────────────────────────────────────────────────────────────

// memorySection is the "memory" object.
func memorySection() (map[string]any, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info := map[string]int64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(parts[1])
		valStr = strings.TrimSuffix(valStr, " kB")
		val, _ := strconv.ParseInt(strings.TrimSpace(valStr), 10, 64)
		info[key] = val
	}

	totalMB := info["MemTotal"] / 1024
	availMB := info["MemAvailable"] / 1024
	usedMB := totalMB - availMB
	swapTotalMB := info["SwapTotal"] / 1024
	swapFreeMB := info["SwapFree"] / 1024
	swapUsedMB := swapTotalMB - swapFreeMB
	pct := 0.0
	if totalMB > 0 {
		pct = float64(usedMB) / float64(totalMB) * 100
	}

	return map[string]any{
		"total_mb": totalMB, "used_mb": usedMB,
		"available_mb": availMB, "swap_used_mb": swapUsedMB, "pct": round2(pct),
	}, nil
}

// ── Disk ─────────────────────────────────────────────────────────────────────

// diskRows is one payload per real block-device mount.
func diskRows() ([]map[string]any, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := map[string]bool{}
	var rows []map[string]any

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		device, mountpoint := fields[0], fields[1]
		// Only real block devices.
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		if seen[mountpoint] {
			continue
		}
		seen[mountpoint] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mountpoint, &stat); err != nil {
			continue
		}

		totalGB := float64(stat.Blocks*uint64(stat.Bsize)) / (1 << 30)
		freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1 << 30)
		usedGB := totalGB - freeGB
		pct := 0.0
		if totalGB > 0 {
			pct = usedGB / totalGB * 100
		}

		rows = append(rows, map[string]any{
			"path":     mountpoint,
			"total_gb": round2(totalGB), "used_gb": round2(usedGB), "pct": round2(pct),
		})
	}
	return rows, nil
}

// ── Load ─────────────────────────────────────────────────────────────────────

// loadSection is the "load" object.
func loadSection() (map[string]any, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected /proc/loadavg format")
	}
	load1, _ := strconv.ParseFloat(fields[0], 64)
	load5, _ := strconv.ParseFloat(fields[1], 64)
	load15, _ := strconv.ParseFloat(fields[2], 64)

	return map[string]any{
		"load1": round2(load1), "load5": round2(load5), "load15": round2(load15),
	}, nil
}

// ── Network ──────────────────────────────────────────────────────────────────

func readNetDev() (map[string]netState, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := map[string]netState{}
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // skip headers
		}
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 10 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		result[iface] = netState{rxBytes: rx, txBytes: tx}
	}
	return result, nil
}

// networkRows is one payload per interface present in both samples.
func networkRows(prev, cur map[string]netState, elapsed float64) []map[string]any {
	var rows []map[string]any
	for iface, c := range cur {
		p, ok := prev[iface]
		if !ok {
			continue
		}
		rows = append(rows, map[string]any{
			"iface":        iface,
			"rx_bytes_sec": int64(float64(c.rxBytes-p.rxBytes) / elapsed),
			"tx_bytes_sec": int64(float64(c.txBytes-p.txBytes) / elapsed),
			// Cumulative counters from the current sample.
			"rx_bytes_total": c.rxBytes,
			"tx_bytes_total": c.txBytes,
		})
	}
	return rows
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func round2(f float64) float64 {
	return float64(int(f*100)) / 100
}

// sanitizeDeployment makes a deployment name safe as the service path
// segment host metrics are written under: '/' would add a segment, and
// whitespace and '.' read oddly in a pseudofile path, so each becomes '_'.
// Empty input returns empty.
func sanitizeDeployment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '.', ' ', '\t':
			return '_'
		}
		return r
	}, s)
}

// sendRowWithReconnect sends a pre-built LogRow, reconnecting if needed.
func sendRowWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient,
	ss *streamState, row *pb.LogRow, label string) error {

	row.Deployment = globalDeployment
	row.Region = globalRegion
	stampMetavariables(row, "")

	if ss.stream != nil {
		if err := ss.stream.Send(row); err == nil {
			return nil
		}
		weftsdk.Infof(ctx, "[%s] stream broken, reconnecting", label)
		ss.stream = nil
	}

	stream, err := openStream(ctx, client, label)
	if err != nil {
		return err
	}
	ss.stream = stream

	if err := ss.stream.Send(row); err != nil {
		weftsdk.Errorf(ctx, "[%s] send failed after reconnect: %v", label, err)
		ss.stream = nil
	}
	return nil
}
