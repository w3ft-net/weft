package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// MetricsConfig is the per-source metrics configuration.
type MetricsConfig struct {
	Enabled    bool     `yaml:"enabled"`
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

// netState holds per-interface rx/tx bytes from the previous sample.
type netState struct {
	rxBytes, txBytes int64
}

// runMetricsCollector runs in its own goroutine, collecting system metrics
// at the configured interval and sending them as LogRows through the
// coordinator IngestStream.
func runMetricsCollector(ctx context.Context, client pb.WeftCoordinatorClient,
	customer, instance, namespace string, mcfg MetricsConfig) {

	interval := 30 * time.Second
	if mcfg.Interval != "" {
		d, err := time.ParseDuration(mcfg.Interval)
		if err != nil {
			log.Printf("[%s/metrics] bad interval %q, using 30s", namespace, mcfg.Interval)
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

	metricsApp := namespace + "/metrics"
	if hg := sanitizeHostGroup(globalHostGroup); hg != "" {
		metricsApp = namespace + "/" + hg + "_metrics"
	}
	log.Printf("[%s] metrics enabled: interval=%s collectors=%v", metricsApp, interval, mcfg.Collectors)

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
			now := time.Now()
			elapsed := now.Sub(prevTime).Seconds()
			if prevTime.IsZero() {
				elapsed = 0
			}

			var rows []*pb.LogRow

			if collectors["cpu"] {
				cur, err := readCPU()
				if err == nil {
					if prevCPU != nil {
						rows = append(rows, buildCPURow(metricsApp, customer, instance, now, prevCPU, cur))
					}
					prevCPU = cur
				}
			}

			if collectors["memory"] {
				if row, err := buildMemoryRow(metricsApp, customer, instance, now); err == nil {
					rows = append(rows, row)
				}
			}

			if collectors["disk"] {
				if diskRows, err := buildDiskRows(metricsApp, customer, instance, now); err == nil {
					rows = append(rows, diskRows...)
				}
			}

			if collectors["load"] {
				if row, err := buildLoadRow(metricsApp, customer, instance, now); err == nil {
					rows = append(rows, row)
				}
			}

			if collectors["network"] {
				curNet, err := readNetDev()
				if err == nil && len(prevNet) > 0 && elapsed > 0 {
					rows = append(rows, buildNetworkRows(metricsApp, customer, instance, now, prevNet, curNet, elapsed)...)
				}
				if err == nil {
					prevNet = curNet
				}
			}

			prevTime = now

			for _, row := range rows {
				if err := sendRowWithReconnect(ctx, client, ss, row, metricsApp); err != nil {
					return // context cancelled
				}
			}
		}
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

func buildCPURow(app, customer, instance string, now time.Time, prev, cur *cpuState) *pb.LogRow {
	dt := float64(cur.total() - prev.total())
	if dt == 0 {
		dt = 1
	}
	user := float64(cur.user-prev.user) / dt * 100
	system := float64(cur.system-prev.system) / dt * 100
	idle := float64(cur.idle-prev.idle) / dt * 100
	iowait := float64(cur.iowait-prev.iowait) / dt * 100

	// Count logical CPUs from /proc/stat (cpu0, cpu1, ...).
	cores := countCPUs()

	payload, _ := json.Marshal(map[string]any{
		"type": "cpu", "ts": now.UTC().Format(time.RFC3339Nano),
		"host": instance, "host_group": globalHostGroup,
		"user": round2(user), "system": round2(system),
		"idle": round2(idle), "iowait": round2(iowait), "cores": cores,
	})

	return &pb.LogRow{
		App: app, Customer: customer, Instance: instance,
		Raw: string(payload), Level: "INFO", TsUnixNs: now.UnixNano(),
		PayloadJson: payload,
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

func buildMemoryRow(app, customer, instance string, now time.Time) (*pb.LogRow, error) {
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

	payload, _ := json.Marshal(map[string]any{
		"type": "memory", "ts": now.UTC().Format(time.RFC3339Nano),
		"host": instance, "host_group": globalHostGroup,
		"total_mb": totalMB, "used_mb": usedMB,
		"available_mb": availMB, "swap_used_mb": swapUsedMB, "pct": round2(pct),
	})

	return &pb.LogRow{
		App: app, Customer: customer, Instance: instance,
		Raw: string(payload), Level: "INFO", TsUnixNs: now.UnixNano(),
		PayloadJson: payload,
	}, nil
}

// ── Disk ─────────────────────────────────────────────────────────────────────

func buildDiskRows(app, customer, instance string, now time.Time) ([]*pb.LogRow, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := map[string]bool{}
	var rows []*pb.LogRow

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

		payload, _ := json.Marshal(map[string]any{
			"type": "disk", "ts": now.UTC().Format(time.RFC3339Nano),
			"host": instance, "host_group": globalHostGroup,
			"path": mountpoint,
			"total_gb": round2(totalGB), "used_gb": round2(usedGB), "pct": round2(pct),
		})

		rows = append(rows, &pb.LogRow{
			App: app, Customer: customer, Instance: instance,
			Raw: string(payload), Level: "INFO", TsUnixNs: now.UnixNano(),
			PayloadJson: payload,
		})
	}
	return rows, nil
}

// ── Load ─────────────────────────────────────────────────────────────────────

func buildLoadRow(app, customer, instance string, now time.Time) (*pb.LogRow, error) {
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

	payload, _ := json.Marshal(map[string]any{
		"type": "load", "ts": now.UTC().Format(time.RFC3339Nano),
		"host": instance, "host_group": globalHostGroup,
		"load1": round2(load1), "load5": round2(load5), "load15": round2(load15),
	})

	return &pb.LogRow{
		App: app, Customer: customer, Instance: instance,
		Raw: string(payload), Level: "INFO", TsUnixNs: now.UnixNano(),
		PayloadJson: payload,
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

func buildNetworkRows(app, customer, instance string, now time.Time,
	prev, cur map[string]netState, elapsed float64) []*pb.LogRow {

	var rows []*pb.LogRow
	for iface, c := range cur {
		p, ok := prev[iface]
		if !ok {
			continue
		}
		rxPerSec := float64(c.rxBytes-p.rxBytes) / elapsed
		txPerSec := float64(c.txBytes-p.txBytes) / elapsed

		payload, _ := json.Marshal(map[string]any{
			"type": "network", "ts": now.UTC().Format(time.RFC3339Nano),
			"host": instance, "host_group": globalHostGroup,
			"iface": iface,
			"rx_bytes_sec": int64(rxPerSec), "tx_bytes_sec": int64(txPerSec),
		})

		rows = append(rows, &pb.LogRow{
			App: app, Customer: customer, Instance: instance,
			Raw: string(payload), Level: "INFO", TsUnixNs: now.UnixNano(),
			PayloadJson: payload,
		})
	}
	return rows
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func round2(f float64) float64 {
	return float64(int(f*100)) / 100
}

// sanitizeHostGroup replaces characters that would break the
// {namespace}/{subapp}.log pseudofile convention ('/') or read oddly in a
// filename (whitespace, '.') with '_'. Empty input returns empty.
func sanitizeHostGroup(s string) string {
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

	row.HostGroup = globalHostGroup

	if ss.stream != nil {
		if err := ss.stream.Send(row); err == nil {
			return nil
		}
		log.Printf("[%s] stream broken, reconnecting", label)
		ss.stream = nil
	}

	stream, err := openStream(ctx, client, label)
	if err != nil {
		return err
	}
	ss.stream = stream

	if err := ss.stream.Send(row); err != nil {
		log.Printf("[%s] send failed after reconnect: %v", label, err)
		ss.stream = nil
	}
	return nil
}
