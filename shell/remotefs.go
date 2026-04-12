package shell

import (
	"context"
	"fmt"
	"strings"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shellcore"
)

// RemoteFS implements shellcore.FS by proxying to the coordinator over gRPC.
// /log/ comes from ListApps, /bin/ and /cron.d/ from the function/watch RPCs.
type RemoteFS struct {
	client   pb.WeftCoordinatorClient
	customer string
	apps     []string // cached from ListApps
}

// NewRemoteFS creates a remote FS using the coordinator gRPC client.
func NewRemoteFS(client pb.WeftCoordinatorClient, customer string, apps []string) *RemoteFS {
	return &RemoteFS{client: client, customer: customer, apps: apps}
}

func (f *RemoteFS) ReadFile(path string) ([]byte, error) {
	p := normFSPath(path)
	dir, name := splitFSPath(p)

	switch dir {
	case "bin":
		resp, err := f.client.GetFunction(context.Background(), &pb.GetFunctionRequest{
			Customer: f.customer, Name: name,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: not found", path)
		}
		return []byte(resp.Source), nil

	case "cron.d":
		resp, err := f.client.GetWatch(context.Background(), &pb.GetWatchRequest{
			Customer: f.customer, Name: name,
		})
		if err != nil {
			return nil, fmt.Errorf("%s: not found", path)
		}
		return []byte(serializeCronFile(resp)), nil
	}

	return nil, fmt.Errorf("%s: not found", path)
}

func (f *RemoteFS) WriteFile(path string, data []byte) error {
	p := normFSPath(path)
	dir, name := splitFSPath(p)

	switch dir {
	case "bin":
		_, err := f.client.SaveFunction(context.Background(), &pb.SaveFunctionRequest{
			Customer: f.customer, Name: name, Source: string(data),
		})
		return err

	case "cron.d":
		schedule, pipeline, notifyDest := parseCronFileContent(string(data))
		_, err := f.client.SaveWatch(context.Background(), &pb.SaveWatchRequest{
			Customer: f.customer, Name: name,
			Schedule: schedule, Pipeline: pipeline, NotifyDest: notifyDest,
		})
		return err
	}

	return fmt.Errorf("%s: read-only", path)
}

func (f *RemoteFS) ReadDir(path string) ([]string, error) {
	p := normFSPath(path)

	switch p {
	case "/":
		return []string{"bin/", "cron.d/", "log/", "etc/"}, nil

	case "/log":
		entries := make([]string, len(f.apps))
		for i, app := range f.apps {
			entries[i] = app + ".log"
		}
		return entries, nil

	case "/bin":
		resp, err := f.client.ListFunctions(context.Background(), &pb.ListFunctionsRequest{
			Customer: f.customer,
		})
		if err != nil {
			return []string{}, nil
		}
		return resp.Names, nil

	case "/cron.d":
		resp, err := f.client.ListWatches(context.Background(), &pb.ListWatchesRequest{
			Customer: f.customer,
		})
		if err != nil {
			return []string{}, nil
		}
		return resp.Names, nil

	case "/etc":
		return []string{}, nil
	}

	return nil, fmt.Errorf("%s: no such directory", path)
}

func (f *RemoteFS) Remove(path string) error {
	p := normFSPath(path)
	dir, name := splitFSPath(p)

	switch dir {
	case "bin":
		_, err := f.client.DeleteFunction(context.Background(), &pb.DeleteFunctionRequest{
			Customer: f.customer, Name: name,
		})
		return err
	case "cron.d":
		_, err := f.client.DeleteWatch(context.Background(), &pb.DeleteWatchRequest{
			Customer: f.customer, Name: name,
		})
		return err
	}

	return fmt.Errorf("%s: operation not permitted", path)
}

// ── Helpers ─────────────────────────────────────────────────

// GetWatchResults implements shellcore.WatchResultsProvider.
func (f *RemoteFS) GetWatchResults(name string, limit int) ([]shellcore.WatchResultValue, error) {
	resp, err := f.client.GetWatchResults(context.Background(), &pb.GetWatchResultsRequest{
		Customer: f.customer, Name: name, Limit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	var out []shellcore.WatchResultValue
	for _, r := range resp.Results {
		out = append(out, shellcore.WatchResultValue{
			Value: strings.TrimSpace(r.Stdout),
			RanAt: r.RanAt,
		})
	}
	return out, nil
}

func normFSPath(path string) string {
	if path == "" || path == "." {
		return "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	if len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func splitFSPath(path string) (dir, name string) {
	path = strings.TrimPrefix(path, "/")
	if idx := strings.Index(path, "/"); idx >= 0 {
		return path[:idx], path[idx+1:]
	}
	return path, ""
}

func serializeCronFile(w *pb.GetWatchResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# schedule: %s\n", w.Schedule)
	if w.NotifyDest != "" {
		fmt.Fprintf(&b, "# notify: %s\n", w.NotifyDest)
	}
	if !w.Enabled {
		b.WriteString("# enabled: false\n")
	}
	b.WriteString(w.Pipeline)
	if !strings.HasSuffix(w.Pipeline, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

func parseCronFileContent(content string) (schedule, pipeline, notifyDest string) {
	var pipelineLines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if strings.HasPrefix(trimmed, "# notify:") {
			notifyDest = strings.TrimSpace(strings.TrimPrefix(trimmed, "# notify:"))
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pipelineLines = append(pipelineLines, line)
		}
	}
	pipeline = strings.Join(pipelineLines, "\n")
	return
}
