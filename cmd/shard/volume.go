package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	shardsvc "github.com/w3ft-net/weft/shard/service"
)

// defaultVolumeHelper mounts and unmounts the shard data volume. The shard
// runs as app, so it calls the helper through a sudoers rule that allows
// exactly this path (deploy/shard-volume.sh, installed by the AMI build).
const defaultVolumeHelper = "/usr/local/sbin/weft-shard-volume"

// hostVolume is the production shardsvc.Volume: the root helper does the
// device lookup, mkfs of a blank volume, mount, and unmount.
type hostVolume struct {
	helper string
}

// Mount blocks until the volume is attached and mounted (the helper waits
// for the device), or ctx ends.
func (v hostVolume) Mount(ctx context.Context) error { return v.run(ctx, "mount") }

// Unmount syncs and unmounts the data directory so the leader can detach
// the volume cleanly.
func (v hostVolume) Unmount(ctx context.Context) error { return v.run(ctx, "unmount") }

func (v hostVolume) run(ctx context.Context, verb string) error {
	out, err := exec.CommandContext(ctx, "sudo", "-n", v.helper, verb).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", v.helper, verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isSpare reports whether this instance was launched as a warm-pool spare:
// the coordinator tags spares shard_spare=true and retags a spare when it
// claims it, so a claimed spare that reboots comes back as its shard.
// SHARD_SPARE overrides the tag off-EC2.
func isSpare() bool {
	if v := os.Getenv("SHARD_SPARE"); v != "" {
		return v == "1" || v == "true"
	}
	return imdsTag("shard_spare") == "true"
}

// startsAsSpare reports whether the process should boot waiting for a volume.
// The tag alone is racy: the leader can claim a booting spare, and retag it,
// after startup.sh saw the spare tag and left the data directory unmounted but
// before this process reads the tag. Where the host manages a volume, a data
// directory that is not a mount point means the volume has not arrived, so
// wait for it rather than run a shard on the root disk.
func startsAsSpare(tagged bool, dataDir string, managesVolume bool) bool {
	return tagged || (managesVolume && !isMountPoint(dataDir))
}

// isMountPoint reports whether dir is on a different device than its parent.
func isMountPoint(dir string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(dir, &st) != nil || syscall.Stat(filepath.Dir(filepath.Clean(dir)), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// activateWhenAttached turns a spare into a shard once the leader attaches
// a volume. Activate blocks on the attach; a failure (a mount that errored)
// is retried, since the spare has nothing else to do.
func activateWhenAttached(srv *shardsvc.Server) {
	for {
		err := srv.Activate(context.Background())
		if err == nil {
			return
		}
		weftsdk.Errorf(context.Background(), "spare activation failed, retrying: %v", err)
		time.Sleep(5 * time.Second)
	}
}

// imdsTag reads one instance tag through IMDS (instance_metadata_tags is
// enabled on the shard launch template). "" when absent or off-EC2.
func imdsTag(key string) string {
	client := &http.Client{Timeout: 2 * time.Second}
	tokenReq, _ := http.NewRequest("PUT", "http://169.254.169.254/latest/api/token", nil)
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return ""
	}
	token, _ := io.ReadAll(tokenResp.Body)
	tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		return ""
	}
	req, _ := http.NewRequest("GET", "http://169.254.169.254/latest/meta-data/tags/instance/"+key, nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(token))
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}
