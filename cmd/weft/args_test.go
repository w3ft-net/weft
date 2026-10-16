package main

import (
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in, ns, svc, err string
	}{
		{in: "weft/api", ns: "weft", svc: "api"},
		{in: "weft/shard/main", ns: "weft", svc: "shard/main"}, // service may itself contain a slash
		{in: "weft", err: "ns/svc"},
		{in: "weft/", err: "ns/svc"},
		{in: "/api", err: "ns/svc"},
		{in: "", err: "ns/svc"},
	} {
		ns, svc, err := parseTarget(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("parseTarget(%q) err = %v, want one mentioning %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || ns != tc.ns || svc != tc.svc {
			t.Errorf("parseTarget(%q) = %q, %q, %v; want %q, %q", tc.in, ns, svc, err, tc.ns, tc.svc)
		}
	}
}

func TestParseSourceAddArgs(t *testing.T) {
	a, err := parseSourceAddArgs([]string{"acme/api", "--log-group", "/aws/lambda/x", "--region", "eu-west-1"})
	if err != nil || a.namespace != "acme" || a.service != "api" || a.logGroup != "/aws/lambda/x" || a.region != "eu-west-1" {
		t.Errorf("full form = %+v, %v", a, err)
	}
	a, err = parseSourceAddArgs([]string{"--log-group", "g", "acme/api"}) // positional may come after flags
	if err != nil || a.namespace != "acme" || a.region != "us-east-2" {
		t.Errorf("flags first = %+v, %v (region should default)", a, err)
	}
	for name, args := range map[string][]string{
		"no target":     {"--log-group", "g"},
		"no log group":  {"acme/api"},
		"bare name":     {"api", "--log-group", "g"},
		"two targets":   {"a/b", "c/d", "--log-group", "g"},
		"old namespace": {"--namespace", "acme", "--service", "api", "--log-group", "g"},
	} {
		if _, err := parseSourceAddArgs(args); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}

func TestParseOldFlagsPointAtNewForm(t *testing.T) {
	_, err := parseSourceAddArgs([]string{"--namespace", "acme", "--service", "api", "--log-group", "g"})
	if err == nil || !strings.Contains(err.Error(), "ns/svc") {
		t.Errorf("old flags error = %v, want a hint at ns/svc", err)
	}
}

func TestParseNamespaceArgs(t *testing.T) {
	// token create NS [--label L] [--env E]
	tc, err := parseTokenCreateArgs([]string{"acme", "--label", "prod", "--env", "live"})
	if err != nil || tc.namespace != "acme" || tc.label != "prod" || tc.env != "live" {
		t.Errorf("token create = %+v, %v", tc, err)
	}
	for _, args := range [][]string{{}, {"--namespace", "acme"}, {"--ns", "acme"}, {"acme/api"}, {"a", "b"}} {
		if _, err := parseTokenCreateArgs(args); err == nil {
			t.Errorf("token create accepted %v", args)
		}
	}

	// source pending [NS] [--watch]
	p, err := parseSourcePendingArgs([]string{"acme", "-w"})
	if err != nil || p.namespace != "acme" || !p.watch {
		t.Errorf("pending = %+v, %v", p, err)
	}
	if p, err := parseSourcePendingArgs(nil); err != nil || p.namespace != "" || p.watch {
		t.Errorf("pending no args = %+v, %v", p, err)
	}
	for _, args := range [][]string{{"--namespace", "acme"}, {"-n", "acme"}, {"a", "b"}} {
		if _, err := parseSourcePendingArgs(args); err == nil {
			t.Errorf("pending accepted %v (an ignored filter would list every namespace)", args)
		}
	}

	// namespace remove NS [--force]
	r, err := parseNamespaceRemoveArgs([]string{"acme", "--force"})
	if err != nil || r.namespace != "acme" || !r.force {
		t.Errorf("remove = %+v, %v", r, err)
	}
	for _, args := range [][]string{{}, {"--force"}, {"acme/api"}, {"--name", "acme"}} {
		if _, err := parseNamespaceRemoveArgs(args); err == nil {
			t.Errorf("namespace remove accepted %v", args)
		}
	}

	// namespace create NAME
	if n, err := parseNamespaceCreateArgs([]string{"acme"}); err != nil || n != "acme" {
		t.Errorf("create = %q, %v", n, err)
	}
	for _, args := range [][]string{{}, {"--name", "acme"}, {"a/b"}} {
		if _, err := parseNamespaceCreateArgs(args); err == nil {
			t.Errorf("namespace create accepted %v", args)
		}
	}
}
