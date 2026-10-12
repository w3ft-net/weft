package coordinator

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"
	luapkg "github.com/graybeardcorp/warp/pkg/lua"
)

// Every weft.<fn> in Lua must exist as weft::<fn> in bash, and vice
// versa: the manual documents one surface in two spellings.
func TestWeftSurfaceParity_LuaAndBash(t *testing.T) {
	var out, errb bytes.Buffer
	code := luapkg.RunScript(
		`for k in pairs(weft) do print(k) end`, nil, strings.NewReader(""),
		&out, &errb, luapkg.WithExtension(buildWeftLuaExtension(newMock())))
	if code != 0 {
		t.Fatalf("lua enumerate failed: %s", errb.String())
	}
	lua := strings.Fields(out.String())
	sort.Strings(lua)

	interp := bash.NewInterpreter()
	wf := &WeftFuncs{}
	RegisterWeftCommands(interp, wf)
	RegisterWeftPredicates(interp, wf)

	for _, name := range lua {
		if _, ok := interp.LookupType("weft::" + name); !ok {
			t.Errorf("weft.%s exists in Lua but weft::%s is not a bash command", name, name)
		}
	}
	inLua := map[string]bool{}
	for _, n := range lua {
		inLua[n] = true
	}
	for _, name := range []string{"latest", "previous", "avg", "min", "max", "sum", "stddev", "count", "percentile", "first", "delta", "slope", "rate"} {
		if !inLua[name] {
			t.Errorf("weft::%s documented but weft.%s missing in Lua", name, name)
		}
	}
}
