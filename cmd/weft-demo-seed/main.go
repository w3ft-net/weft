// weft-demo-seed populates a fresh weft database with the
// multi-customer demo scenario described in
// docs/plans/multi-customer-demo-seed.md.
//
// Idempotent: every step uses FirstOrCreate / "skip if exists" so
// re-running is safe. Token re-mint is gated by tokens.env existing —
// if it does, no new tokens are written; if it doesn't, twelve fresh
// tokens are minted and the file is created.
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
}

// errorRatios mirrors shapes — a couple of "noisy" services give the
// alerting/wit demos something to fire on.
var errorRatios = map[shapeKey]float64{
	{"acme", "api", "prod"}:          0.08,
	{"globex", "billing", "staging"}: 0.12,
	{"globex", "billing", "prod"}:    0.10,
	// everything else: default 0.05 inside entrypoint.sh
}

// --- the seed ---------------------------------------------------------

func seed(s *store.Store, tokensOut string) error {
	tokensExist := fileExists(tokensOut)

	var tokenLines []mintedToken

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

		// Agent tokens — one per (app, env). Skipped if tokens.env already exists.
		for _, app := range c.Apps {
			for _, env := range c.Envs {
				varName := tokenEnvVar(c.Name, app, env)
				if tokensExist {
					continue
				}
				label := fmt.Sprintf("%s-%s-%s", c.Name, app, env)
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
	}

	if !tokensExist {
		if err := writeTokensFile(tokensOut, tokenLines); err != nil {
			return err
		}
	} else {
		log.Printf("tokens.env already present at %s — leaving in place", tokensOut)
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
		Tier:       "swatch",
		Status:     "active",
		StartsAt:   now,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

func upsertVerifiedUser(s *store.Store, email string, customerID uint) (*store.User, error) {
	if u, err := s.GetUserByEmailAndCustomer(email, customerID); err == nil && u != nil {
		return u, nil
	}
	return s.CreateVerifiedUser(email, customerID)
}

func minimalSchemaYAML(app string) string {
	// Keep the schema permissive — the demo logs are unstructured-ish; we
	// rely on awk/grep more than on field projection. The schema just
	// needs to be parseable so AddLogGroup accepts it.
	return fmt.Sprintf(`fields:
  - name: line
    type: string
app: %s
`, app)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// --- pre-seeded /bin and /cron.d --------------------------------------

func preseedScripts(s *store.Store, custID uint, customerName string, members []*store.User) error {
	if len(members) < 2 {
		return nil
	}
	user1, user2 := members[0], members[1]

	// Pick a primary app per customer so the canned watch points at a
	// path that resolves. Both customers carry the same set of apps in
	// the demo, so this is just a per-tenant choice for "the app worth
	// watching" — not a generic mapping.
	primaryApp := map[string]string{
		"acme":   "acme/api",
		"globex": "globex/orders",
	}[customerName]
	if primaryApp == "" {
		return fmt.Errorf("preseedScripts: no primary app configured for customer %q", customerName)
	}
	primaryLog := "/log/" + primaryApp + ".log"

	// /bin/scan: three versions, alternating authors → wit log shows the
	// back-and-forth that the wit feature was built for.
	//
	// Idempotency: the version-history trigger fires on every UPDATE
	// targeting `source`, including same-value writes. Re-running the
	// v1→v2→v3 playback therefore captures three new history rows each
	// time. Guard with a "live row already at final version" check —
	// once the trail is laid down on first seed, subsequent runs are
	// no-ops.
	scanV1 := fmt.Sprintf("cat %s | grep -i error | wc -l", primaryLog)
	scanV2 := fmt.Sprintf("cat %s | grep ERROR | wc -l", primaryLog)
	scanV3 := fmt.Sprintf("cat %s | awk '$LEVEL == \"ERROR\"' | head -100 | wc -l", primaryLog)
	if existing, _ := s.GetShellFunction(custID, "scan"); existing == nil || existing.Source != scanV3 {
		// Each save is followed by a HEAD-amend so the version
		// dropping into history on the *next* save carries the
		// commit message (the trigger captures OLD.message).
		versions := []struct {
			userID, source, message string
		}{
			{user1.ID, scanV1, "initial pass"},
			{user2.ID, scanV2, "tighten error pattern"},
			{user1.ID, scanV3, "fix the awk filter"},
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

	// /bin/count_errors: single version.
	countErrors := "cat /log/$1 | grep ERROR | wc -l"
	if existing, _ := s.GetShellFunction(custID, "count_errors"); existing == nil {
		if err := s.SaveShellFunction(custID, user2.ID, "count_errors", countErrors); err != nil {
			return fmt.Errorf("count_errors: %w", err)
		}
		if err := s.AmendShellFunctionMessage(custID, "count_errors", "HEAD", "count ERROR lines in any log"); err != nil {
			return fmt.Errorf("count_errors message: %w", err)
		}
	}

	// /cron.d/error-rate: two versions — a narrow per-tick window via
	// tail, then widened. The watch's TimeRange is unbounded today
	// (cron.go:150-153), so a bare `cat` would count every error
	// since ingest started; tail -N bounds the slice to the most
	// recent rows so the value reflects the rate-shape generator's
	// current ERROR_RATIO and varies tick-to-tick. v2 must NOT
	// reference its own past results — weft.avg over a freshly seeded
	// watch emits empty stdout, which `alert` then rejects as
	// "non-numeric value", failing every tick.
	rateV1 := fmt.Sprintf("tail -n 500 %s | grep ERROR | wc -l | alert above 50", primaryLog)
	rateV2 := fmt.Sprintf("tail -n 2000 %s | grep ERROR | wc -l | alert above 200", primaryLog)
	if existing, _ := s.GetCronWatch(custID, "error-rate"); existing == nil || existing.Pipeline != rateV2 {
		watches := []struct {
			userID, pipeline, message string
		}{
			{user1.ID, rateV1, "5m error count"},
			{user2.ID, rateV2, "widen to 1h for a smoother chart"},
		}
		for i, v := range watches {
			if err := s.SaveCronWatch(custID, v.userID, "error-rate", "*/5 * * * *", v.pipeline); err != nil {
				return fmt.Errorf("error-rate v%d: %w", i+1, err)
			}
			if err := s.AmendCronWatchMessage(custID, "error-rate", "HEAD", v.message); err != nil {
				return fmt.Errorf("error-rate v%d message: %w", i+1, err)
			}
		}
	}
	return nil
}

type mintedToken struct {
	varName string
	raw     string
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
