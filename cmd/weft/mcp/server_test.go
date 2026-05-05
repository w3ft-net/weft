package mcp

import (
	"context"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testSession builds an in-memory client/server pair against a
// Config the test supplies. Tests call methods on the returned
// *mcp.ClientSession to invoke tools just as a real client would.
//
// The cleanup is t.Cleanup so individual subtests don't have to
// remember to close anything; the session is torn down when the
// test ends.
func testSession(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()

	server := NewServer(cfg)
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// callTool wraps the verbose CallTool boilerplate. Returns the raw
// result for the test to assert against.
func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// textContent concatenates every TextContent block in a result.
// Tests assert on the joined text rather than individual blocks
// so the layout (e.g. data block + notice block) is allowed to
// evolve without churning every fixture.
func textContent(res *mcp.CallToolResult) string {
	var s string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s += tc.Text
		}
	}
	return s
}

// neverRunFn is a RunFunc that fails the test if invoked. Use it
// when the test asserts the policy validator should reject input
// before any execution happens.
func neverRunFn(t *testing.T) RunFunc {
	t.Helper()
	return func(_ string, _ string, _ io.Writer) error {
		t.Errorf("RunFunc must not be called for this fixture")
		return nil
	}
}
