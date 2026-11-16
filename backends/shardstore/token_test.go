package shardstore

import "testing"

func TestConstantTimeTokenEqual(t *testing.T) {
	tok, err := GenerateShardHeartbeatToken()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := GenerateShardHeartbeatToken()
	cases := []struct {
		name            string
		stored, present string
		want            bool
	}{
		{"same", tok, tok, true},
		{"different", tok, other, false},
		{"shorter", tok, tok[:len(tok)-1], false},
		// An empty stored token means there is nothing to authenticate against.
		{"both empty", "", "", false},
		{"empty stored", "", tok, false},
		{"empty presented", tok, "", false},
	}
	for _, c := range cases {
		if got := ConstantTimeTokenEqual(c.stored, c.present); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
