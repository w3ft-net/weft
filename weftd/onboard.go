package weftd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/graybeardcorp/weft/internal/weftlog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

const (
	sampleLineCount = 50
	pollInterval    = 5 * time.Second
	// progressInterval bounds how often the agent pings the coordinator
	// with a partial sample while still accumulating toward the target.
	progressInterval = 30 * time.Second
)

// SourceStatus is the onboarding status returned by the coordinator.
type SourceStatus struct {
	Status            string // "unknown", "sampling", "pending", "configured"
	SchemaYAML        string // populated when Status == "configured"
	SchemaHash        string // SHA-256 hex of the YAML; empty when not configured
	ResampleRequested bool   // user asked for a fresh sample (configured, or waiting on a schema)
	SampleCollected   int32  // accumulation progress while Status == "sampling"
	SampleTarget      int32
}

// AgentVersion and AgentInstance identify this agent (its release and its
// host) to the coordinator on every status poll. Set once at startup; the
// coordinator uses them to tell which agents can read a given schema.
var AgentVersion, AgentInstance string

// CheckSourceStatus queries the coordinator for the configuration status of a source.
func CheckSourceStatus(ctx context.Context, client pb.WeftCoordinatorClient, token, app string) (*SourceStatus, error) {
	resp, err := client.GetSourceStatus(ctx, &pb.GetSourceStatusRequest{
		Token:        token,
		Service:      app,
		AgentVersion: AgentVersion,
		Instance:     AgentInstance,
	})
	if err != nil {
		return nil, fmt.Errorf("get source status: %w", err)
	}
	return &SourceStatus{
		Status:            resp.Status,
		SchemaYAML:        resp.SchemaYaml,
		SchemaHash:        resp.SchemaHash,
		ResampleRequested: resp.ResampleRequested,
		SampleCollected:   resp.SampleCollected,
		SampleTarget:      resp.SampleTarget,
	}, nil
}

// sampleSize is the line count to collect when the coordinator asked for
// target lines; it asks for none (0) outside a resample.
func sampleSize(target int) int {
	if target <= 0 {
		return sampleLineCount
	}
	return target
}

// SubmitSample accumulates a full sample of target lines (0 means
// sampleLineCount) from a file and sends it to the coordinator. While still short of the target it ships throttled partial
// progress pings so the source surfaces as "sampling (N/target)" rather
// than stalling invisibly on a low-volume source. fresh skips the file's
// existing content and samples only lines written from now on (a resample
// wants current traffic, not the startup chatter at the head of the file).
func SubmitSample(ctx context.Context, client pb.WeftCoordinatorClient, token, app, path, instance, deployment string, fresh bool, target int) error {
	target = sampleSize(target)
	progress := func(lines []string) {
		// Best-effort: a dropped progress ping just delays the UI update.
		_ = submitSampleLines(ctx, client, token, app, instance, deployment, lines, true)
	}
	lines, err := SampleLines(ctx, path, target, fresh, progress)
	if err != nil {
		return fmt.Errorf("sample %s: %w", path, err)
	}
	if len(lines) < target {
		// Only reachable when ctx was cancelled before the full sample
		// accumulated (agent shutdown). Do not finalize a partial sample.
		return ctx.Err()
	}
	return submitSampleLines(ctx, client, token, app, instance, deployment, lines, false)
}

// SubmitSDKSample waits for the file's first line and, when it carries a weft
// SDK header, submits it as the final sample. An SDK-fed source needs no
// extraction, so there is nothing to gain from collecting a full sample, and
// a quiet source would otherwise sit unshipped until it wrote that many
// lines. It reports whether it submitted; when it did not (the first line is
// plain), nothing was sent beyond progress pings and the caller samples as
// usual.
func SubmitSDKSample(ctx context.Context, client pb.WeftCoordinatorClient, token, app, path, instance, deployment string) (bool, error) {
	progress := func(lines []string) {
		_ = submitSampleLines(ctx, client, token, app, instance, deployment, lines, true)
	}
	first, err := SampleLines(ctx, path, 1, false, progress)
	if err != nil {
		return false, fmt.Errorf("sample %s: %w", path, err)
	}
	if len(first) == 0 {
		return false, ctx.Err()
	}
	if _, sdkFed, _ := StripSample(first); !sdkFed {
		return false, nil
	}
	return true, submitSampleLines(ctx, client, token, app, instance, deployment, first, false)
}

// SubmitUnitSample is the journald (unit:) counterpart of SubmitSample.
// Journald sources have no file path, so the file sampler cannot serve
// them — it would open("") and fail with ENOENT.
func SubmitUnitSample(ctx context.Context, client pb.WeftCoordinatorClient, token, app, unit, instance, deployment string, fresh bool, target int) error {
	target = sampleSize(target)
	progress := func(lines []string) {
		_ = submitSampleLines(ctx, client, token, app, instance, deployment, lines, true)
	}
	lines, err := SampleUnitLines(ctx, unit, target, fresh, progress)
	if err != nil {
		return fmt.Errorf("sample unit %s: %w", unit, err)
	}
	if len(lines) < target {
		return ctx.Err()
	}
	return submitSampleLines(ctx, client, token, app, instance, deployment, lines, false)
}

// submitSampleLines ships a sample to the coordinator. partial=true marks a
// progress ping (still accumulating); partial=false is the final, complete
// sample that makes the source configurable. Shared by the file and
// journald sampling paths.
func submitSampleLines(ctx context.Context, client pb.WeftCoordinatorClient, token, app, instance, deployment string, lines []string, partial bool) error {
	// A source written by the weft SDK has a header on each line. Send the
	// bodies and say so, so the coordinator configures it without a wizard.
	lines, sdkFed, sdkFormat := StripSample(lines)
	_, err := client.SubmitSample(ctx, &pb.SubmitSampleRequest{
		Token:      token,
		Service:    app,
		Instance:   instance,
		Deployment: deployment,
		Lines:      lines,
		Partial:    partial,
		SdkFed:     sdkFed,
		SdkFormat:  sdkFormat,
	})
	if err != nil {
		return fmt.Errorf("submit sample: %w", err)
	}
	return nil
}

// SampleUnitLines accumulates up to target non-empty lines from a systemd
// unit's journal. It seeds from the unit's backlog and then follows the
// journal (journalctl -f) until it has target lines or ctx is done. A quiet
// unit blocks (per design: wait for a full sample) but reports progress via
// the optional progress callback. On ctx cancellation, or if journalctl
// exits early (e.g. the unit has very little history), it returns the lines
// collected so far with no error.
func SampleUnitLines(ctx context.Context, unit string, target int, fresh bool, progress func([]string)) ([]string, error) {
	backlog := target
	if fresh {
		backlog = 0
	}
	cmd := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", strconv.Itoa(backlog), "--no-pager", "-o", "cat", "-f")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("journalctl -u %s: %w", unit, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("journalctl -u %s: %w", unit, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	done := make(chan struct{})
	defer close(done)
	linesCh := make(chan Line)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			select {
			case linesCh <- Line{Text: sc.Text()}:
			case <-done:
				return
			}
		}
		close(linesCh)
	}()

	return accumulate(ctx, target, progress, linesCh, nil), nil
}

// SampleLines accumulates up to target non-empty lines from a file: it reads
// the existing content, then follows the file for new lines until it has
// target lines or ctx is done. A low-volume file blocks (per design: wait
// for a full sample) but reports progress via the optional progress
// callback. On ctx cancellation it returns the lines collected so far with
// no error. With fresh set the existing content is skipped and only lines
// written after the call are sampled.
func SampleLines(ctx context.Context, path string, target int, fresh bool, progress func([]string)) ([]string, error) {
	// Wait for the file to exist (it may be created after the agent starts).
	var f *os.File
	for {
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(pollInterval):
		}
	}

	// Read whatever is already there, unless a fresh sample was asked for.
	var lines []string
	if !fresh {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() && len(lines) < target {
			if line := scanner.Text(); line != "" {
				lines = append(lines, line)
			}
		}
	}
	f.Close()
	if len(lines) >= target {
		return lines, nil
	}

	// Register the source promptly even if it has emitted little, so the
	// coordinator can report "not enough lines yet (N/target)".
	if progress != nil {
		progress(append([]string(nil), lines...))
	}

	// Follow the file for the rest. Starting at EOF, lines written
	// in the tiny window between the read above and this open may be
	// missed, which is acceptable for a sample.
	tailer, err := NewTailer(path, -1)
	if err != nil {
		// We just opened this file successfully, so a tailer failure is
		// unexpected. Surface it rather than returning a partial sample
		// that would never finalize (the caller would poll forever).
		return lines, fmt.Errorf("tail %s: %w", path, err)
	}
	defer tailer.Close()

	return accumulate(ctx, target, progress, tailer.Lines(), lines), nil
}

// accumulate reads non-empty lines from ch (seeded with already-collected
// lines) until it has target of them or ctx is done, emitting throttled
// progress. It returns the lines collected, which may be fewer than target
// if ch closed or ctx was cancelled.
func accumulate(ctx context.Context, target int, progress func([]string), ch <-chan Line, seed []string) []string {
	lines := seed
	tick := time.NewTicker(progressInterval)
	defer tick.Stop()
	for len(lines) < target {
		select {
		case <-ctx.Done():
			return lines
		case line, ok := <-ch:
			if !ok {
				return lines
			}
			if line.Text != "" {
				lines = append(lines, line.Text)
			}
		case <-tick.C:
			if progress != nil {
				progress(append([]string(nil), lines...))
			}
		}
	}
	return lines
}

// WaitForConfiguration polls the coordinator until a source is configured,
// then returns the schema YAML. Blocks until configured or context is cancelled.
// If the user asks for a fresh sample while the source waits (the first
// batch didn't show what the schema needs), resample is called to collect
// and submit one; the wait then continues.
func WaitForConfiguration(ctx context.Context, client pb.WeftCoordinatorClient, token, app string, resample func(ctx context.Context, target int) error) (string, error) {
	for {
		status, err := CheckSourceStatus(ctx, client, token, app)
		if err != nil {
			weftlog.Errorf(ctx, "[%s] poll error: %v — retrying", app, err)
		} else if status.Status == "configured" {
			return status.SchemaYAML, nil
		} else if status.ResampleRequested && resample != nil {
			weftlog.Infof(ctx, "[%s] resample requested — sampling fresh lines", app)
			if err := resample(ctx, int(status.SampleTarget)); err != nil {
				return "", err
			}
			weftlog.Infof(ctx, "[%s] fresh sample submitted — waiting for configuration", app)
			continue
		} else {
			weftlog.Infof(ctx, "[%s] status: %s — waiting for configuration", app, status.Status)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
