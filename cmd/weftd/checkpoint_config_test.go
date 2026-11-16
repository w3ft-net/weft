package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/weftd"
)

func resetCheckpointGlobals(t *testing.T) {
	t.Helper()
	oldCk, oldCfg := checkpoints, shipCfg
	t.Cleanup(func() {
		checkpoints.Close()
		checkpoints, shipCfg = oldCk, oldCfg
	})
	checkpoints = nil
}

func TestCheckpointConfig_ParsesBlockAndOlderAgentsIgnoreIt(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte(`
token: wft_x
checkpoint:
  dir: /tmp/ck
  flush_interval: 2s
  commit_interval: 30s
  max_resume_bytes: 64MiB
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Checkpoint
	if c.Dir != "/tmp/ck" || c.FlushInterval != 2*time.Second || c.CommitInterval != 30*time.Second || c.MaxResumeBytes != "64MiB" {
		t.Errorf("parsed %+v", c)
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{"1048576": 1 << 20, "64KiB": 64 << 10, "256MiB": 256 << 20, "2GiB": 2 << 30, " 8MiB ": 8 << 20} {
		if got, err := parseByteSize(in); err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-5MiB", "0", "12XB", "1.5MiB"} {
		if _, err := parseByteSize(in); err == nil {
			t.Errorf("parseByteSize(%q) should fail", in)
		}
	}
}

func TestConfigureCheckpointing_OpensStoreAndAppliesIntervals(t *testing.T) {
	resetCheckpointGlobals(t)
	dir := t.TempDir()
	flush := configureCheckpointing(CheckpointConfig{Dir: dir, FlushInterval: 3 * time.Second, CommitInterval: 20 * time.Second})
	if checkpoints == nil {
		t.Fatal("store not opened")
	}
	if flush != 3*time.Second || shipCfg.commitInterval != 20*time.Second {
		t.Errorf("flush=%v commit=%v", flush, shipCfg.commitInterval)
	}
}

func TestConfigureCheckpointing_HeldLockDisablesInsteadOfFailing(t *testing.T) {
	resetCheckpointGlobals(t)
	dir := t.TempDir()
	other, err := weftd.OpenCheckpointStore(dir, weftd.CheckpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	configureCheckpointing(CheckpointConfig{Dir: dir})
	if checkpoints != nil {
		t.Error("a second weftd must run without checkpointing, not share the directory")
	}
}

func TestConfigureCheckpointing_UnwritableDirDisablesInsteadOfFailing(t *testing.T) {
	resetCheckpointGlobals(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	configureCheckpointing(CheckpointConfig{Dir: filepath.Join(blocker, "ck")}) // parent is a file
	if checkpoints != nil {
		t.Error("an unusable state dir must not be fatal or half-open")
	}
}

func TestConfigureCheckpointing_BadMaxResumeBytesFallsBackToDefault(t *testing.T) {
	resetCheckpointGlobals(t)
	configureCheckpointing(CheckpointConfig{Dir: t.TempDir(), MaxResumeBytes: "lots"})
	if checkpoints == nil {
		t.Error("a bad max_resume_bytes must not disable checkpointing or be fatal")
	}
}
