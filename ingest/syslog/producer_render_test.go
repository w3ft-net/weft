package syslog

import "testing"

func TestRenderProducerMapFields(t *testing.T) {
	for _, tc := range []struct {
		name             string
		f                Frame
		service, kind, i string
		ok               bool
	}{
		{
			name:    "web instance",
			f:       Frame{Hostname: "render-capture-testsvc", AppName: "web-9nk6q", MsgID: "web-9nk6q"},
			service: "render-capture-testsvc", kind: "web", i: "9nk6q", ok: true,
		},
		{
			name:    "cron instance",
			f:       Frame{Hostname: "test-service", AppName: "cron-stqq5", MsgID: "cron-stqq5"},
			service: "test-service", kind: "cron", i: "stqq5", ok: true,
		},
		{
			name:    "build output kept as its own kind",
			f:       Frame{Hostname: "test-service", AppName: "bld-a1b2c", MsgID: "bld-a1b2c"},
			service: "test-service", kind: "bld", i: "a1b2c", ok: true,
		},
		{
			name:    "falls back to MSGID when APP-NAME absent",
			f:       Frame{Hostname: "test-service", AppName: "", MsgID: "web-9nk6q"},
			service: "test-service", kind: "web", i: "9nk6q", ok: true,
		},
		{
			name: "connectivity-test ping is dropped",
			f:    Frame{Hostname: "render", AppName: "web-9nk6q", MsgID: "web-9nk6q"},
			ok:   false,
		},
		{
			name: "empty hostname is dropped",
			f:    Frame{Hostname: "", AppName: "web-9nk6q"},
			ok:   false,
		},
		{
			name: "app-name with no hyphen is dropped",
			f:    Frame{Hostname: "test-service", AppName: "web", MsgID: "web"},
			ok:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, kind, instance, ok := (RenderProducer{}).MapFields(tc.f)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if service != tc.service || kind != tc.kind || instance != tc.i {
				t.Errorf("MapFields(%+v) = (%q, %q, %q), want (%q, %q, %q)",
					tc.f, service, kind, instance, tc.service, tc.kind, tc.i)
			}
		})
	}
}

func TestRenderProducerName(t *testing.T) {
	if got := (RenderProducer{}).Name(); got != "render" {
		t.Errorf("Name() = %q, want %q", got, "render")
	}
}
