package logdb

import (
	"context"
	"testing"
)

type nopLauncher struct{}

func (nopLauncher) LaunchInstance(context.Context, string) (string, error)  { return "", nil }
func (nopLauncher) TerminateInstance(context.Context, string, string) error { return nil }

// Start runs over any Platform: with only a launcher it builds a provisioner and
// stops with its context, naming no cloud.
func TestStart_OverAPlatformWithOnlyALauncher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov, err := Start(Config{Ctx: ctx}, Platform{Launcher: nopLauncher{}, Config: DevProvisionerConfig()})
	if err != nil || prov == nil {
		t.Fatalf("Start = %v, %v", prov, err)
	}
}

func TestStart_RefusesAPlatformWithNoLauncher(t *testing.T) {
	if _, err := Start(Config{Ctx: context.Background()}, Platform{}); err == nil {
		t.Fatal("Start accepted a platform with no launcher")
	}
}
