package coordinator

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// A grouped pipeline reaches compute as a GroupSpec, never as the legacy
// aggregate string, on both dispatch paths.
func TestExecuteQuery_GroupedPipelineSendsAGroupSpec(t *testing.T) {
	for _, tc := range []struct {
		pipeline string
		want     pb.GroupRender
	}{
		{`cat /log/vimgym.log | wc -l`, pb.GroupRender_GROUP_RENDER_TOTAL},
		{`cat /log/vimgym.log | sort -u`, pb.GroupRender_GROUP_RENDER_LINE},
		{`cat /log/vimgym.log | sort | uniq -c`, pb.GroupRender_GROUP_RENDER_COUNT_LINE},
	} {
		t.Run(tc.pipeline, func(t *testing.T) {
			c := setupTestCoordinator(t)
			pool := &spyCompute{inner: c.Compute}
			c.Compute = pool

			res, err := c.Execute(context.Background(), tc.pipeline, fullRange, "kevlar", "")
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			drainExec(t, res)
			checkGroupRequest(t, "interactive", pool, tc.want)

			cust, err := c.Store.GetOrgByName("kevlar")
			if err != nil {
				t.Fatal(err)
			}
			lp, err := planner.Lower(tc.pipeline, 1, "bolt", noBinResolver{})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			lowered, err := json.Marshal(lp)
			if err != nil {
				t.Fatal(err)
			}
			pool.lastReq = nil
			w := &store.CronWatch{OrgID: cust.ID, Name: "group-watch", Schedule: "every 1m", Pipeline: tc.pipeline, Enabled: true, LoweredPlan: lowered}
			res, err = c.ExecuteForWatch(context.Background(), tc.pipeline, fullRange, w)
			if err != nil {
				t.Fatalf("ExecuteForWatch: %v", err)
			}
			drainExec(t, res)
			checkGroupRequest(t, "lowered watch", pool, tc.want)
		})
	}
}

func checkGroupRequest(t *testing.T, path string, pool *spyCompute, want pb.GroupRender) {
	t.Helper()
	if pool.lastReq == nil {
		t.Fatalf("%s: ExecuteQuery was never called", path)
	}
	if got := pool.lastReq.GetGroup().GetRender(); got != want {
		t.Errorf("%s: group render = %s, want %s", path, got, want)
	}
	if pool.lastReq.Aggregate != "" {
		t.Errorf("%s: aggregate = %q, want none: the group is the instruction", path, pool.lastReq.Aggregate)
	}
}
