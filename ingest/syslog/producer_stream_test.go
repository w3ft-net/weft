package syslog

import (
	"testing"

	"github.com/w3ft-net/weft/sources"
)

func TestRenderProducerToken(t *testing.T) {
	p := RenderProducer{ListenPort: 9023}
	if p.Port() != 9023 {
		t.Errorf("Port = %d", p.Port())
	}
	for _, tc := range []struct {
		name, line, token, frame string
		wantErr                  bool
	}{
		{name: "token then frame", line: "wft_abc <134>1 2026-09-02T00:00:00Z host app - - - hi\n", token: "wft_abc", frame: "<134>1 2026-09-02T00:00:00Z host app - - - hi\n"},
		{name: "crlf kept on the frame", line: "wft_abc <134>1 x\r\n", token: "wft_abc", frame: "<134>1 x\r\n"},
		{name: "a bare frame has no token", line: "<134>1 2026-09-02T00:00:00Z host app - - - hi\n", wantErr: true},
		{name: "an empty line", line: "\n", wantErr: true},
		{name: "no space after the token", line: "wft_abc\n", wantErr: true},
	} {
		token, frame, err := p.Token([]byte(tc.line))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
			continue
		}
		if err == nil && (token != tc.token || string(frame) != tc.frame) {
			t.Errorf("%s: token %q frame %q, want %q %q", tc.name, token, frame, tc.token, tc.frame)
		}
	}
}

// A StreamProducer is a producer: its sources start the way every push source does.
func TestRenderProducerIsAStreamProducerWithPushRules(t *testing.T) {
	var p StreamProducer = RenderProducer{}
	start := p.InitialState(sources.Registration{})
	if start.Status != sources.Configured || !start.ResampleRequested {
		t.Errorf("start = %+v", start)
	}
}
