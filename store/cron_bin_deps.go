package store

import (
	"errors"
	"regexp"

	"gorm.io/gorm"

	"github.com/graybeardcorp/weft/logreads"
)

// CronBinDep says a cron runs a /bin script, directly or through another script.
type CronBinDep struct {
	OrgID       OrgID `gorm:"not null"`
	CronWatchID uint  `gorm:"primaryKey"`
	FunctionID  uint  `gorm:"primaryKey"`
}

func (CronBinDep) TableName() string { return "cron_bin_deps" }

// binEnv describes the org's scripts to the analyzer.
func (s *Store) binEnv(orgID OrgID) (logreads.Env, error) {
	nss, err := s.NamespaceNames(orgID, "", true)
	if err != nil {
		return logreads.Env{}, err
	}
	return logreads.Env{
		Namespaces: nss,
		Bin: func(name string) (string, bool, bool) {
			fn, err := s.GetShellFunction(orgID, name)
			if err != nil || fn == nil {
				return "", false, false
			}
			return fn.Source, fn.Language == "lua", true
		},
	}, nil
}

// recordCronBinDeps replaces the scripts cronID is recorded as running with
// those its pipeline runs now. A pipeline the analyzer can't resolve records
// none: a cron in a namespace is refused at save for that, and an org-level one
// is a gap until it is moved.
func (s *Store) recordCronBinDeps(orgID OrgID, cronID uint, pipeline string) error {
	env, err := s.binEnv(orgID)
	if err != nil {
		return err
	}
	var deps []CronBinDep
	if reads, err := logreads.Analyze(pipeline, env); err == nil {
		for _, name := range reads.Bins {
			if fn, err := s.GetShellFunction(orgID, name); err == nil {
				deps = append(deps, CronBinDep{OrgID: orgID, CronWatchID: cronID, FunctionID: fn.ID})
			}
		}
	}
	return s.crossTenant().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ? AND cron_watch_id = ?", orgID, cronID).Delete(&CronBinDep{}).Error; err != nil {
			return err
		}
		if len(deps) == 0 {
			return nil
		}
		return tx.Create(&deps).Error
	})
}

// CronsUsingBin lists the live crons that run the script, however indirectly:
// those recorded as depending on it, and those whose pipeline names /bin/<name>
// even though the script doesn't exist yet, so that creating it can't slip past
// the crons waiting for it.
func (s *Store) CronsUsingBin(orgID OrgID, name string) ([]CronWatch, error) {
	var crons []CronWatch
	err := s.crossTenant().Raw(`
		SELECT w.* FROM cron_watches w
		 WHERE w.org_id = ? AND w.deleted_at IS NULL
		   AND (EXISTS (SELECT 1 FROM cron_bin_deps d JOIN shell_functions f ON f.id = d.function_id
		                 WHERE d.cron_watch_id = w.id AND f.name = ? AND f.deleted_at IS NULL)
		        OR w.pipeline LIKE ?)
		 ORDER BY w.name`, orgID, name, "%/bin/"+name+"%").Scan(&crons).Error
	if err != nil {
		return nil, err
	}
	// The LIKE is a prefilter: /bin/scan also matches /bin/scan2.
	named := regexp.MustCompile(`/bin/` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_.-])`)
	out := crons[:0]
	for _, c := range crons {
		if named.MatchString(c.Pipeline) || s.recordedBinDep(orgID, c.ID, name) {
			out = append(out, c)
		}
	}
	return out, nil
}

// recordedBinDep reports whether cronID is recorded as running the script.
func (s *Store) recordedBinDep(orgID OrgID, cronID uint, name string) bool {
	var n int64
	s.crossTenant().Raw(`SELECT count(*) FROM cron_bin_deps d JOIN shell_functions f ON f.id = d.function_id
		WHERE d.org_id = ? AND d.cron_watch_id = ? AND f.name = ? AND f.deleted_at IS NULL`, orgID, cronID, name).Scan(&n)
	return n > 0
}

// refreshDependents re-records the scripts of every cron that runs the script,
// after the script changed what it calls.
func (s *Store) refreshDependents(orgID OrgID, name string) error {
	crons, err := s.CronsUsingBin(orgID, name)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range crons {
		errs = append(errs, s.recordCronBinDeps(orgID, c.ID, c.Pipeline))
	}
	return errors.Join(errs...)
}

// backfillCronBinDeps records the scripts of every live cron that has none.
func (s *Store) backfillCronBinDeps() {
	var rows []struct {
		ID       uint
		OrgID    OrgID
		Pipeline string
	}
	if err := s.crossTenant().Raw(`
		SELECT w.id, w.org_id, w.pipeline FROM cron_watches w
		 WHERE w.deleted_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM cron_bin_deps d WHERE d.cron_watch_id = w.id)`).Scan(&rows).Error; err != nil {
		return
	}
	for _, r := range rows {
		_ = s.recordCronBinDeps(r.OrgID, r.ID, r.Pipeline)
	}
}
