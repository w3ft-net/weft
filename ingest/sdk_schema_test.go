package ingest

import "testing"

func TestSDKSchema(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"json", "json"}, {"text", "text"}, {"", "text"}, {"yaml", "text"}} {
		y, err := SDKSchema("api", tc.in)
		if err != nil {
			t.Fatal(err)
		}
		s, err := LoadSchemaFromBytes([]byte(y))
		if err != nil {
			t.Fatalf("schema does not load back: %v\n%s", err, y)
		}
		if !s.SDK || s.Format != tc.want || s.Service != "api" || s.Envelope != "none" {
			t.Errorf("SDKSchema(api, %q) = %+v, want an SDK %s schema", tc.in, s, tc.want)
		}
		if problems := s.Validate(); len(problems) != 0 {
			t.Errorf("SDKSchema(%q) fails validation: %v", tc.in, problems)
		}
	}
}
