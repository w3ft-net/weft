package weftd

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

const (
	sampleLineCount = 50
	pollInterval    = 5 * time.Second
)

// SourceStatus is the onboarding status returned by the coordinator.
type SourceStatus struct {
	Status     string // "unknown", "pending", "configured"
	SchemaYAML string // populated when Status == "configured"
}

// CheckSourceStatus queries the coordinator for the configuration status of a source.
func CheckSourceStatus(ctx context.Context, client pb.WeftCoordinatorClient, token, app string) (*SourceStatus, error) {
	resp, err := client.GetSourceStatus(ctx, &pb.GetSourceStatusRequest{
		Token: token,
		App:   app,
	})
	if err != nil {
		return nil, fmt.Errorf("get source status: %w", err)
	}
	return &SourceStatus{
		Status:     resp.Status,
		SchemaYAML: resp.SchemaYaml,
	}, nil
}

// SubmitSample reads the first N lines from a file and sends them to the coordinator.
func SubmitSample(ctx context.Context, client pb.WeftCoordinatorClient, token, app, path, instance, deployment string) error {
	lines, err := SampleLines(path, sampleLineCount)
	if err != nil {
		return fmt.Errorf("sample %s: %w", path, err)
	}
	if len(lines) == 0 {
		return fmt.Errorf("sample %s: no lines read", path)
	}

	_, err = client.SubmitSample(ctx, &pb.SubmitSampleRequest{
		Token:      token,
		App:        app,
		Instance:   instance,
		Deployment: deployment,
		Lines:      lines,
	})
	if err != nil {
		return fmt.Errorf("submit sample: %w", err)
	}
	return nil
}

// SampleLines reads up to n lines from a file. If the file doesn't exist yet
// or is empty, it waits briefly for content to appear.
func SampleLines(path string, n int) ([]string, error) {
	// Wait up to 30s for the file to have content.
	var lines []string
	for attempt := 0; attempt < 6; attempt++ {
		f, err := os.Open(path)
		if err != nil {
			if attempt < 5 {
				time.Sleep(5 * time.Second)
				continue
			}
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() && len(lines) < n {
			line := scanner.Text()
			if line != "" {
				lines = append(lines, line)
			}
		}
		f.Close()
		if len(lines) > 0 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return lines, nil
}

// WaitForConfiguration polls the coordinator until a source is configured,
// then returns the schema YAML. Blocks until configured or context is cancelled.
func WaitForConfiguration(ctx context.Context, client pb.WeftCoordinatorClient, token, app string) (string, error) {
	for {
		status, err := CheckSourceStatus(ctx, client, token, app)
		if err != nil {
			log.Printf("[%s] poll error: %v — retrying", app, err)
		} else if status.Status == "configured" {
			return status.SchemaYAML, nil
		} else {
			log.Printf("[%s] status: %s — waiting for configuration", app, status.Status)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
