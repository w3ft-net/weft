package weftd

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/internal/weftlog"
)

// DetectRegion probes the standard cloud-metadata endpoints in order
// (AWS IMDSv2 → GCP metadata server → Azure IMDS) and returns the
// first region it gets. Returns "" if none are reachable; queries
// on REGION simply don't filter for that agent.
//
// Honors `override` if non-empty — for bare-metal / lab hosts that
// have no metadata service but still want a region label.
//
// Each probe has a short timeout; metadata services are link-local
// and either respond fast or aren't there.
func DetectRegion(ctx context.Context, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}

	probes := []func(context.Context) string{
		probeAWS,
		probeGCP,
		probeAzure,
	}
	for _, p := range probes {
		if r := p(ctx); r != "" {
			return r
		}
	}
	return ""
}

// probeAWS uses IMDSv2 — token-based auth, mandatory since 2023.
func probeAWS(ctx context.Context) string {
	tokenReq := mustReq(ctx, "PUT", "http://169.254.169.254/latest/api/token", nil)
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	token, ok := doProbe(tokenReq)
	if !ok {
		return ""
	}

	regReq := mustReq(ctx, "GET", "http://169.254.169.254/latest/meta-data/placement/region", nil)
	regReq.Header.Set("X-aws-ec2-metadata-token", token)
	region, _ := doProbe(regReq)
	return region
}

// probeGCP returns the zone, then strips the trailing "-a"/"-b"/etc
// to get the region. GCP's region is "us-central1" not
// "us-central1-a".
func probeGCP(ctx context.Context) string {
	req := mustReq(ctx, "GET", "http://metadata.google.internal/computeMetadata/v1/instance/zone", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	zone, ok := doProbe(req)
	if !ok {
		return ""
	}
	// Format: "projects/<id>/zones/us-central1-a" → "us-central1-a" → "us-central1".
	if i := strings.LastIndex(zone, "/"); i >= 0 {
		zone = zone[i+1:]
	}
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// probeAzure uses Azure IMDS. The "location" field is the Azure
// region (e.g., "eastus2").
func probeAzure(ctx context.Context) string {
	req := mustReq(ctx, "GET", "http://169.254.169.254/metadata/instance/compute/location?api-version=2021-02-01&format=text", nil)
	req.Header.Set("Metadata", "true")
	region, _ := doProbe(req)
	return region
}

// doProbe runs a single HTTP probe with a short timeout. Returns
// (body, true) on a 2xx, ("", false) on any failure. Probes never
// log — failures are expected when the agent isn't on that cloud.
func doProbe(req *http.Request) (string, bool) {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(body)), true
}

// mustReq is a panic-free http.NewRequestWithContext wrapper. The URL
// inputs above are all hard-coded and well-formed, so an error here
// means a programmer typo — log and return a request that will fail
// in doProbe.
func mustReq(ctx context.Context, method, url string, body io.Reader) *http.Request {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		weftlog.Infof(ctx, "region probe: bad request %s %s: %v", method, url, err)
		// Return a stub that doProbe will fail on cleanly.
		return &http.Request{}
	}
	return req
}
