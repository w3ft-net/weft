// weft-demo-seed populates a fresh weft database with the
// multi-customer demo scenario described in
// docs/plans/multi-customer-demo-seed.md.
//
// Idempotent: every step uses FirstOrCreate / "skip if exists" so
// re-running is safe. Tokens are always freshly minted (previous
// tokens with the same label are revoked first), and written to the
// output file.
//
// Usage:
//   weft-demo-seed --db-dsn "host=localhost port=5433 ..." \
//                  --tokens-out dev/demo/tokens.env
//   weft-demo-seed --reset    # delete acme + globex (cascades)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevlarcon/weft/store"
)

func main() {
	dsn := flag.String("db-dsn", os.Getenv("DB_DSN"), "postgres DSN; falls back to $DB_DSN")
	tokensOut := flag.String("tokens-out", "dev/demo/tokens.env", "path to write minted token env vars to")
	reset := flag.Bool("reset", false, "delete demo customers (cascades through users, namespaces, log_groups, tokens, scripts, watches) and exit")
	flag.Parse()

	if *dsn == "" {
		log.Fatal("--db-dsn or DB_DSN required")
	}

	s, err := store.Open(*dsn)
	if err != nil {
		log.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	if *reset {
		if err := resetDemo(s); err != nil {
			log.Fatalf("reset: %v", err)
		}
		log.Print("demo customers deleted")
		return
	}

	if err := seed(s, *tokensOut); err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Printf("seed complete; tokens written to %s", *tokensOut)
}

// --- the spec ---------------------------------------------------------

type teamMember struct {
	Email           string
	Name            string
	ReadOnly        bool
	CanCreateNS     bool
}

type customerSpec struct {
	Name        string
	DisplayName string
	Members     []teamMember
	Envs        []string // e.g. ["sand", "prod"]
	Apps        []string // e.g. ["api", "worker", "edge"]
}

// shape per (customer, app, env) keyed for the compose overlay's env-var lookup.
type shapeKey struct{ customer, app, env string }

var spec = []customerSpec{
	{
		Name:        "acme",
		DisplayName: "Acme Robotics",
		Envs:        []string{"sand", "prod"},
		Apps:        []string{"api", "worker", "edge"},
		Members: []teamMember{
			{Email: "alice@acme.test", Name: "Alice Chen", CanCreateNS: true},
			{Email: "bob@acme.test", Name: "Bob Patel"},
			{Email: "carol@acme.test", Name: "Carol Reyes", ReadOnly: true},
		},
	},
	{
		Name:        "globex",
		DisplayName: "Globex Foods",
		Envs:        []string{"staging", "prod"},
		Apps:        []string{"orders", "billing", "notifier"},
		Members: []teamMember{
			{Email: "dana@globex.test", Name: "Dana Okafor", CanCreateNS: true},
			{Email: "eli@globex.test", Name: "Eli Bergstrom"},
			{Email: "frida@globex.test", Name: "Frida Lukoja", ReadOnly: true},
		},
	},
	// Loom Inc is the populated demo org every signup gets a read-only
	// membership in (api/onboarding.go's grantLoomMembership). The
	// onboarding tour walks the user through Loom's data, so this
	// customer needs the richest fixture set — multiple busy services,
	// pre-seeded bin/ + cron.d/ + notify.conf + dashboards. The
	// docker-compose.demo.yml producer containers feed it live data.
	{
		Name:        "loom",
		DisplayName: "Loom Inc",
		Envs:        []string{"sand", "prod"},
		Apps:        []string{"api", "worker", "edge"},
		Members: []teamMember{
			{Email: "ops@loom.test", Name: "Loom Ops", CanCreateNS: true},
			{Email: "kai@loom.test", Name: "Kai Nakamura"},
			{Email: "priya@loom.test", Name: "Priya Sharma"},
		},
	},
}

var shapes = map[shapeKey]string{
	{"acme", "api", "sand"}:           "const:8",
	{"acme", "api", "prod"}:           "sine:20,40,600",
	{"acme", "worker", "sand"}:        "sawtooth:2,0.05,12",
	{"acme", "worker", "prod"}:        "sine:5,10,300",
	{"acme", "edge", "sand"}:          "const:3",
	{"acme", "edge", "prod"}:          "sine:10,15,180",
	{"globex", "orders", "staging"}:   "sawtooth:1,0.02,8",
	{"globex", "orders", "prod"}:      "sine:30,60,900",
	{"globex", "billing", "staging"}:  "monotonic:1,0.02",
	{"globex", "billing", "prod"}:     "monotonic:5,0.05,40",
	{"globex", "notifier", "staging"}: "const:4",
	{"globex", "notifier", "prod"}:    "sine:8,16,420",
	// Loom traffic shapes — picked so a tour user can find ERROR
	// lines in api/prod (8% errors), see latency variation in worker,
	// and see steady "boring" traffic in edge.
	{"loom", "api", "sand"}:    "const:6",
	{"loom", "api", "prod"}:    "sine:25,50,600",
	{"loom", "worker", "sand"}: "const:3",
	{"loom", "worker", "prod"}: "sawtooth:5,0.08,30",
	{"loom", "edge", "sand"}:   "const:2",
	{"loom", "edge", "prod"}:   "const:6",
}

// errorRatios mirrors shapes — a couple of "noisy" services give the
// alerting/wit demos something to fire on.
var errorRatios = map[shapeKey]float64{
	{"acme", "api", "prod"}:          0.08,
	{"globex", "billing", "staging"}: 0.12,
	{"globex", "billing", "prod"}:    0.10,
	{"loom", "api", "prod"}:          0.08, // tour finds ERROR lines here
	{"loom", "worker", "prod"}:       0.04,
	// everything else: default 0.05 inside entrypoint.sh
}

// --- the seed ---------------------------------------------------------

func seed(s *store.Store, tokensOut string) error {
	var tokenLines []mintedToken
	var nonLoomUsers []nonLoomUser

	for _, c := range spec {
		log.Printf("customer %s …", c.Name)
		cust, err := s.FindOrCreateCustomer(c.Name)
		if err != nil {
			return fmt.Errorf("customer %s: %w", c.Name, err)
		}
		if err := s.UpdateCustomerDisplayName(c.Name, c.DisplayName); err != nil {
			return fmt.Errorf("customer %s display name: %w", c.Name, err)
		}

		if err := ensureActiveSubscription(s, c.Name); err != nil {
			return fmt.Errorf("subscription %s: %w", c.Name, err)
		}

		ns, err := s.FindOrCreateNamespace(cust.ID, c.Name)
		if err != nil {
			return fmt.Errorf("namespace %s: %w", c.Name, err)
		}

		// Users
		var members []*store.User
		for _, m := range c.Members {
			u, err := upsertVerifiedUser(s, m.Email, cust.ID)
			if err != nil {
				return fmt.Errorf("user %s: %w", m.Email, err)
			}
			if err := s.SetUserName(u.ID, m.Name); err != nil {
				return fmt.Errorf("user %s name: %w", m.Email, err)
			}
			if err := s.SetCanCreateNamespace(u.ID, m.CanCreateNS); err != nil {
				return fmt.Errorf("user %s can_create_namespace: %w", m.Email, err)
			}
			if m.ReadOnly {
				if err := s.UpsertNamespacePermission(&store.NamespacePermission{
					UserID:      u.ID,
					NamespaceID: ns.ID,
					CanRead:     true,
				}); err != nil {
					return fmt.Errorf("user %s perm: %w", m.Email, err)
				}
			} else {
				if err := s.GrantFullNamespaceAccess(u.ID, ns.ID); err != nil {
					return fmt.Errorf("user %s grant: %w", m.Email, err)
				}
			}
			members = append(members, u)
		}

		// Log groups (one per app, weftd source, no AWS account). The
		// `app` column stores the full namespace/app form for weftd
		// sources — that's what the coordinator's configured-source
		// lookup matches against (store.SourceIsConfigured).
		for _, app := range c.Apps {
			fullApp := c.Name + "/" + app
			if err := s.AddLogGroup(ns.ID, nil, store.SourceTypeWeftd,
				fullApp, fullApp, "us-east-2", minimalSchemaYAML(app), "30s"); err != nil {
				return fmt.Errorf("log_group %s: %w", fullApp, err)
			}
		}

		// Agent tokens — one per (app, env). Revoke any previous tokens
		// with the same label so we don't accumulate orphans across restarts.
		for _, app := range c.Apps {
			for _, env := range c.Envs {
				varName := tokenEnvVar(c.Name, app, env)
				label := fmt.Sprintf("%s-%s-%s", c.Name, app, env)
				_ = s.RevokeAgentTokensByLabel(cust.ID, label)
				raw, _, err := s.CreateAgentToken(cust.ID, ns.ID, label, env)
				if err != nil {
					return fmt.Errorf("token %s: %w", label, err)
				}
				tokenLines = append(tokenLines, mintedToken{varName: varName, raw: raw})
			}
		}

		// Pre-seeded /bin and /cron.d content — give the team an initial
		// state with multi-author wit history. Pass the customer slug
		// so the canned scripts/watches can reference paths that
		// actually resolve in this customer's namespace.
		if err := preseedScripts(s, cust.ID, c.Name, members); err != nil {
			return fmt.Errorf("preseed %s: %w", c.Name, err)
		}

		if c.Name != "loom" {
			for _, u := range members {
				nonLoomUsers = append(nonLoomUsers, nonLoomUser{user: u, email: u.Email})
			}
		}
	}

	if err := grantLoomMemberships(s, nonLoomUsers); err != nil {
		return fmt.Errorf("loom memberships: %w", err)
	}

	if err := writeTokensFile(tokensOut, tokenLines); err != nil {
		return err
	}
	return nil
}

func tokenEnvVar(customer, app, env string) string {
	// e.g. ACME_API_SAND_TOKEN
	return strings.ToUpper(fmt.Sprintf("%s_%s_%s_TOKEN", customer, app, env))
}

func writeTokensFile(path string, lines []mintedToken) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Generated by weft-demo-seed; do not edit by hand.\n")
	b.WriteString("# Re-mint by `rm dev/demo/tokens.env && weft-demo-seed --reset && weft-demo-seed`.\n\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "%s=%s\n", l.varName, l.raw)
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func ensureActiveSubscription(s *store.Store, customerName string) error {
	if sub, _ := s.ActiveSubscription(customerName); sub != nil {
		return nil
	}
	now := time.Now()
	return s.CreateSubscription(&store.Subscription{
		ID:         "demo-sub-" + customerName,
		CustomerID: customerName,
		// Yard tier: allows the 1-minute mem-pressure schedule
		// the demo's verification checklist depends on. Swatch caps
		// watches at 5-minute intervals (store.MinWatchInterval).
		Tier:       "yard",
		Status:     "active",
		StartsAt:   now,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

func upsertVerifiedUser(s *store.Store, email string, customerID uint) (*store.User, error) {
	if u, err := s.GetUserByEmail(email); err == nil && u != nil {
		// Ensure the user has a team_member row in this customer so the
		// demo seed treats them as an org member (the seed reruns
		// idempotently; CreateTeamMember below upserts on the unique
		// (customer_id, user_id) constraint).
		if _, err := s.GetTeamMemberByUserID(customerID, u.ID); err != nil {
			now := time.Now()
			_ = s.CreateTeamMember(&store.TeamMember{
				ID:         "demo-tm-" + u.ID + "-" + fmt.Sprint(customerID),
				CustomerID: customerID,
				Email:      u.Email,
				UserID:     &u.ID,
				Role:       "member",
				Status:     "active",
				InvitedAt:  now,
				CreatedAt:  now,
				UpdatedAt:  now,
			})
		}
		return u, nil
	}
	u, err := s.CreateVerifiedUser(email)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if err := s.CreateTeamMember(&store.TeamMember{
		ID:         "demo-tm-" + u.ID + "-" + fmt.Sprint(customerID),
		CustomerID: customerID,
		Email:      u.Email,
		UserID:     &u.ID,
		Role:       "owner",
		Status:     "active",
		InvitedAt:  now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		return nil, fmt.Errorf("team_member: %w", err)
	}
	return u, nil
}

func minimalSchemaYAML(app string) string {
	// The generator emits one line per request/job/order/etc. as
	// `<ts> <LEVEL> <action> key=value …` — same shape across api,
	// worker, edge, orders, billing, notifier. Extracting the
	// timestamp + level into real columns is what makes
	// `awk '$LEVEL == "ERROR"'` push down to a SQL `level = 'ERROR'`
	// predicate (otherwise it filters on a column that doesn't
	// exist and silently returns 0). The metrics streams emitted
	// by weftd are JSONL with no level field; their schema is
	// auto-discovered on first ingest, not seeded here.
	return fmt.Sprintf(`fields:
  - name: line
    type: string
timestamp_field: "1"
timestamp_format: "2006-01-02T15:04:05.000Z"
level_field: "2"
app: %s
`, app)
}

// --- pre-seeded /bin and /cron.d --------------------------------------

func preseedScripts(s *store.Store, custID uint, customerName string, members []*store.User) error {
	if len(members) < 3 {
		return nil
	}
	user1, user2, user3 := members[0], members[1], members[2]

	primaryApp := map[string]string{
		"acme":   "acme/api",
		"globex": "globex/orders",
		"loom":   "loom/api",
	}[customerName]
	if primaryApp == "" {
		return fmt.Errorf("preseedScripts: no primary app configured for customer %q", customerName)
	}
	primaryLog := "/log/" + primaryApp + ".log"
	primaryEnv := map[string]string{
		"acme":   "prod",
		"globex": "prod",
		"loom":   "prod",
	}[customerName]
	metricsLog := fmt.Sprintf("/log/%s/%s/%s_metrics.log",
		customerName, primaryEnv, lastSegment(primaryApp))

	// --- /bin/scan: three versions, three authors -------------------------
	//
	// Idempotency: the version-history trigger fires on every UPDATE
	// targeting `source`, including same-value writes. Guard with a
	// "live row already at final version" check.
	scanV1 := fmt.Sprintf("cat %s | grep -i error | wc -l", primaryLog)
	scanV2 := fmt.Sprintf("cat %s | grep ERROR | wc -l", primaryLog)
	scanV3 := fmt.Sprintf("cat %s | awk '$LEVEL == \"ERROR\"' | head -100 | wc -l", primaryLog)
	if existing, _ := s.GetShellFunction(custID, "scan"); existing == nil || existing.Source != scanV3 {
		versions := []struct {
			userID, source, message string
		}{
			{user1.ID, scanV1, "initial pass — case-insensitive grep"},
			{user2.ID, scanV2, "tighten to exact ERROR match"},
			{user3.ID, scanV3, "use awk pushdown so shards filter server-side"},
		}
		for i, v := range versions {
			if err := s.SaveShellFunction(custID, v.userID, "scan", v.source); err != nil {
				return fmt.Errorf("scan v%d: %w", i+1, err)
			}
			if err := s.AmendShellFunctionMessage(custID, "scan", "HEAD", v.message); err != nil {
				return fmt.Errorf("scan v%d message: %w", i+1, err)
			}
		}
	}

	// --- /bin/count_errors: two versions ----------------------------------
	countV1 := "cat /log/$1 | grep ERROR | wc -l"
	countV2 := "cat /log/$1 | awk '$LEVEL == \"ERROR\"' | wc -l"
	if existing, _ := s.GetShellFunction(custID, "count_errors"); existing == nil || existing.Source != countV2 {
		versions := []struct {
			userID, source, message string
		}{
			{user2.ID, countV1, "count ERROR lines in any log"},
			{user3.ID, countV2, "switch to awk for pushdown"},
		}
		for i, v := range versions {
			if err := s.SaveShellFunction(custID, v.userID, "count_errors", v.source); err != nil {
				return fmt.Errorf("count_errors v%d: %w", i+1, err)
			}
			if err := s.AmendShellFunctionMessage(custID, "count_errors", "HEAD", v.message); err != nil {
				return fmt.Errorf("count_errors v%d message: %w", i+1, err)
			}
		}
	}

	// --- /bin/mem_history: three versions ---------------------------------
	memV1 := fmt.Sprintf("tail -n 100 %s | jq '.pct'", metricsLog)
	memV2 := fmt.Sprintf("tail -n 360 %s | jq 'select(.type == \"memory\") | {ts, pct}'", metricsLog)
	memV3 := "tail -n 360 $1 | jq 'select(.type == \"memory\") | {ts, pct}'"
	if existing, _ := s.GetShellFunction(custID, "mem_history"); existing == nil || existing.Source != memV3 {
		versions := []struct {
			userID, source, message string
		}{
			{user1.ID, memV1, "quick memory dump"},
			{user2.ID, memV2, "add timestamp, filter to memory type"},
			{user3.ID, memV3, "take path as $1 so it works for any metrics log"},
		}
		for i, v := range versions {
			if err := s.SaveShellFunction(custID, v.userID, "mem_history", v.source); err != nil {
				return fmt.Errorf("mem_history v%d: %w", i+1, err)
			}
			if err := s.AmendShellFunctionMessage(custID, "mem_history", "HEAD", v.message); err != nil {
				return fmt.Errorf("mem_history v%d message: %w", i+1, err)
			}
		}
	}

	// --- /bin/severity: lua via shebang -----------------------------------
	//
	// Demonstrates the shebang-marked Lua path (detectLanguage in
	// store/shell.go strips the `#!` line and stores Language="lua").
	// Computes a weighted severity score from a log stream — the kind
	// of accumulator that's awkward in pure bash but two lines of
	// Lua. The tour's `shell-bin-lua` lesson points here.
	severityV1 := `#!/usr/bin/lua
-- Weighted severity sum from a log stream. ERROR=1, WARN=0.1.
-- See /bin/severity for the version-history; ` + "`wit log /bin/severity`" + `.
local total = 0
for line in lines() do
  if line:find("ERROR") then total = total + 1
  elseif line:find("WARN") then total = total + 0.1
  end
end
print(string.format("%.1f", total))
`
	if existing, _ := s.GetShellFunction(custID, "severity"); existing == nil || existing.Source != strings.TrimPrefix(severityV1, "#!/usr/bin/lua\n") {
		if err := s.SaveShellFunction(custID, user2.ID, "severity", severityV1); err != nil {
			return fmt.Errorf("severity: %w", err)
		}
		if err := s.AmendShellFunctionMessage(custID, "severity", "HEAD", "lua-driven severity weighting"); err != nil {
			return fmt.Errorf("severity message: %w", err)
		}
	}

	// --- /bin/status.lua: lua via filename extension ----------------------
	//
	// The .lua suffix is enough for detectLanguage to mark this as
	// Lua — no shebang. Demonstrates table-as-map + sort, a shape
	// that bash needs awkward awk for.
	statusLua := `-- Tally HTTP status codes from request lines, ascending order.
local counts = {}
for line in lines() do
  local code = line:match("status=(%d+)")
  if code then counts[code] = (counts[code] or 0) + 1 end
end
local keys = {}
for k in pairs(counts) do keys[#keys+1] = k end
table.sort(keys)
for _, k in ipairs(keys) do print(k, counts[k]) end
`
	if existing, _ := s.GetShellFunction(custID, "status.lua"); existing == nil || existing.Source != statusLua {
		if err := s.SaveShellFunction(custID, user3.ID, "status.lua", statusLua); err != nil {
			return fmt.Errorf("status.lua: %w", err)
		}
		if err := s.AmendShellFunctionMessage(custID, "status.lua", "HEAD", "lua via .lua extension; no shebang needed"); err != nil {
			return fmt.Errorf("status.lua message: %w", err)
		}
	}

	// --- /cron.d/error-rate: three versions -------------------------------
	rateV1 := fmt.Sprintf("tail -n 500 %s | grep ERROR | wc -l | alert above 50", primaryLog)
	rateV2 := fmt.Sprintf("tail -n 2000 %s | grep ERROR | wc -l | alert above 200", primaryLog)
	rateV3 := fmt.Sprintf("tail -n 2000 %s | awk '$LEVEL == \"ERROR\"' | wc -l | alert above 200", primaryLog)
	if existing, _ := s.GetCronWatch(custID, "error-rate"); existing == nil || existing.Pipeline != rateV3 {
		versions := []struct {
			userID, pipeline, message string
		}{
			{user1.ID, rateV1, "5m error count with low threshold"},
			{user2.ID, rateV2, "widen window to reduce flapping"},
			{user3.ID, rateV3, "switch to awk for shard pushdown"},
		}
		for i, v := range versions {
			if err := s.SaveCronWatch(custID, v.userID, "error-rate", "*/5 * * * *", v.pipeline); err != nil {
				return fmt.Errorf("error-rate v%d: %w", i+1, err)
			}
			if err := s.AmendCronWatchMessage(custID, "error-rate", "HEAD", v.message); err != nil {
				return fmt.Errorf("error-rate v%d message: %w", i+1, err)
			}
		}
	}

	// --- /cron.d/mem-pressure: two versions -------------------------------
	memPressureV1 := fmt.Sprintf(
		"tail -n 12 %s | jq 'select(.type == \"memory\") | .pct' | alert above 80",
		metricsLog)
	memPressureV2 := fmt.Sprintf(
		"tail -n 12 %s | jq 'select(.type == \"memory\")' "+
			"| alert above 70 -k .pct -t .host",
		metricsLog)
	if existing, _ := s.GetCronWatch(custID, "mem-pressure"); existing == nil || existing.Pipeline != memPressureV2 {
		versions := []struct {
			userID, pipeline, message string
		}{
			{user1.ID, memPressureV1, "basic memory threshold"},
			{user2.ID, memPressureV2, "lower to 70, add per-host tag so alerts identify the box"},
		}
		for i, v := range versions {
			if err := s.SaveCronWatch(custID, v.userID, "mem-pressure", "*/1 * * * *", v.pipeline); err != nil {
				return fmt.Errorf("mem-pressure v%d: %w", i+1, err)
			}
			if err := s.AmendCronWatchMessage(custID, "mem-pressure", "HEAD", v.message); err != nil {
				return fmt.Errorf("mem-pressure v%d message: %w", i+1, err)
			}
		}
	}

	// --- /etc/dashboards/prod ---------------------------------------------
	prodDashboard := `title: Production overview
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        watch: error-rate
        title: Error count (5m)
        size: lg
      - kind: counter
        watch: error-rate
        title: Current errors
        size: sm
        threshold:
          warn: 50
          crit: 200
  - title: Memory
    tiles:
      - kind: gauge
        watch: mem-pressure
        title: Memory pressure
        size: md
        threshold:
          warn: 70
          crit: 90
          max: 100
`
	if err := s.SaveDashboard(custID, user1.ID, "prod", prodDashboard); err != nil {
		return fmt.Errorf("dashboard prod: %w", err)
	}

	// --- /etc/notify.yaml -------------------------------------------------
	notifyYAML := `ops:
  webhook:
    - "https://hooks.loom.test/weft/ops"
  email:
    - "ops@loom.test"
    - "kai@loom.test"

oncall:
  webhook:
    - url: "https://hooks.loom.test/weft/oncall"
      headers:
        X-Signing-Secret: "loom-demo-secret"
  email:
    - "oncall@loom.test"
`
	if err := s.SaveNotifyAliases(custID, notifyYAML); err != nil {
		return fmt.Errorf("notify.yaml: %w", err)
	}

	return nil
}

// lastSegment returns the suffix after the final "/", or the whole
// string if there is none. Used to derive the bare app name (e.g.
// "api" from "acme/api") for metrics-log path construction.
func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

type mintedToken struct {
	varName string
	raw     string
}

type nonLoomUser struct {
	user  *store.User
	email string
}

func grantLoomMemberships(s *store.Store, users []nonLoomUser) error {
	cust, err := s.GetCustomerByName("loom")
	if err != nil {
		return fmt.Errorf("loom customer: %w", err)
	}
	ns, err := s.GetNamespace(cust.ID, "loom")
	if err != nil {
		return fmt.Errorf("loom namespace: %w", err)
	}
	for _, u := range users {
		if _, err := s.GetTeamMemberByUserID(cust.ID, u.user.ID); err == nil {
			continue
		}
		now := time.Now()
		if err := s.CreateTeamMember(&store.TeamMember{
			ID:         "demo-loom-" + u.user.ID,
			CustomerID: cust.ID,
			Email:      u.email,
			UserID:     &u.user.ID,
			Role:       "member",
			Status:     "active",
			InvitedAt:  now,
			CreatedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			return fmt.Errorf("loom team_member %s: %w", u.email, err)
		}
		if err := s.UpsertNamespacePermission(&store.NamespacePermission{
			UserID:      u.user.ID,
			NamespaceID: ns.ID,
			CanRead:     true,
		}); err != nil {
			return fmt.Errorf("loom perm %s: %w", u.email, err)
		}
		log.Printf("  granted loom membership to %s", u.email)
	}
	return nil
}

// --- reset ------------------------------------------------------------

func resetDemo(s *store.Store) error {
	for _, c := range spec {
		if err := s.DeleteCustomerByName(c.Name); err != nil {
			return fmt.Errorf("delete customer %s: %w", c.Name, err)
		}
		log.Printf("deleted customer %s (cascade)", c.Name)
	}
	return nil
}
