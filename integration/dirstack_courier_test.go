//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestDirstackCourier_GRPC drives the live coordinator's Execute RPC with
// a seeded `weft-dirstack` metadata value and asserts (a) the server
// resolved the pipeline against that cwd and (b) it returned the post-run
// dirstack as a `weft-final-dirstack` trailer. This is the wire half of
// the dirstack courier — the delegated client ferries its cwd in, reads
// the new cwd out, and caches it.
func TestDirstackCourier_GRPC(t *testing.T) {
	const customer = "courier-grpc"
	seedCustomerWithNamespace(t, customer, "app")
	t.Cleanup(func() { deleteCustomer(t, customer) })

	conn, client := grpcClientAs(t, customer)
	defer conn.Close()

	run := func(t *testing.T, pipeline string, dirStack []string) (string, []string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, d := range dirStack {
			ctx = metadata.AppendToOutgoingContext(ctx, "weft-dirstack", d)
		}
		var trailer metadata.MD
		stream, err := client.Execute(ctx, &pb.CoordinatorExecuteRequest{
			Pipeline: pipeline,
			Customer: customer,
		}, grpc.Trailer(&trailer))
		if err != nil {
			t.Fatalf("Execute(%q): %v", pipeline, err)
		}
		var sb strings.Builder
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			for _, line := range resp.Lines {
				sb.WriteString(line.Raw)
				sb.WriteByte('\n')
			}
		}
		return strings.TrimSpace(sb.String()), trailer.Get("weft-final-dirstack")
	}

	t.Run("seeded cwd is visible and echoed back", func(t *testing.T) {
		out, final := run(t, "pwd", []string{"/log/" + customer})
		if want := "/log/" + customer; out != want {
			t.Errorf("pwd = %q, want %q", out, want)
		}
		if want := []string{"/log/" + customer}; !equalStrings(final, want) {
			t.Errorf("weft-final-dirstack trailer = %v, want %v", final, want)
		}
	})

	t.Run("cd in the pipeline moves the returned cwd", func(t *testing.T) {
		out, final := run(t, "cd /bin && pwd", []string{"/tmp"})
		if out != "/bin" {
			t.Errorf("cd /bin && pwd = %q, want /bin", out)
		}
		if want := []string{"/bin"}; !equalStrings(final, want) {
			t.Errorf("weft-final-dirstack trailer = %v, want %v", final, want)
		}
	})

	t.Run("no seed defaults to root", func(t *testing.T) {
		out, final := run(t, "pwd", nil)
		if out != "/" {
			t.Errorf("pwd (no seed) = %q, want /", out)
		}
		if want := []string{"/"}; !equalStrings(final, want) {
			t.Errorf("weft-final-dirstack trailer = %v, want %v", final, want)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
