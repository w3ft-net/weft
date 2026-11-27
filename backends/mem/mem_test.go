package mem

import (
	"context"
	"github.com/w3ft-net/weft/backends/cron/cronstoretest"
	"github.com/w3ft-net/weft/backends/shardstore/shardstoretest"
	"github.com/w3ft-net/weft/backends/source/sourcestoretest"
	"github.com/w3ft-net/weft/backends/tmp/tmpstoretest"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/backends/token/tokenstoretest"
	"github.com/w3ft-net/weft/shape"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/coordination/coordtest"
	"testing"

	"github.com/w3ft-net/weft/backends/bin/binstoretest"
	"github.com/w3ft-net/weft/backends/dashboard/dashboardstoretest"
	"github.com/w3ft-net/weft/backends/ingestfail/ingeststoretest"
	"github.com/w3ft-net/weft/backends/login/loginstoretest"
	"github.com/w3ft-net/weft/backends/notice/noticestoretest"
	"github.com/w3ft-net/weft/backends/notify/notifystoretest"
)

func TestBinConformance(t *testing.T) {
	binstoretest.Run(t, func(*testing.T) binstoretest.Subject { return binstoretest.Subject{Store: NewBins()} })
}

func TestNoticeConformance(t *testing.T) {
	noticestoretest.Run(t, func(*testing.T) noticestoretest.Subject {
		return noticestoretest.Subject{Store: NewNotices(), User: "u1", Other: "u2"}
	})
}

func TestIngestFailConformance(t *testing.T) {
	ingeststoretest.Run(t, func(*testing.T) ingeststoretest.Subject { return ingeststoretest.Subject{Store: NewIngestFailures()} })
}

func TestDashboardConformance(t *testing.T) {
	dashboardstoretest.Run(t, func(*testing.T) dashboardstoretest.Subject {
		return dashboardstoretest.Subject{Store: NewDashboards()}
	})
}

func TestLoginConformance(t *testing.T) {
	loginstoretest.Run(t, func(*testing.T) loginstoretest.Subject {
		s := NewLogin()
		return loginstoretest.Subject{Store: s, Allow: func(_ *testing.T, email string) { s.Allow(email) }}
	})
}

func TestNotifyConformance(t *testing.T) {
	notifystoretest.Run(t, func(*testing.T) notifystoretest.Subject {
		s := NewNotify()
		return notifystoretest.Subject{Aliases: s, Secrets: s, User: "u1", OtherUser: "u2"}
	})
}

func TestCoordinationConformance(t *testing.T) {
	coordtest.Run(t, func(*testing.T) coordtest.Subject {
		c := NewCoordination()
		return coordtest.Subject{
			C: c,
			Backdate: func(key string, age time.Duration) error {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.claims[key] = time.Now().Add(-age)
				return nil
			},
			// Memory does not outlive the process, so a "restart" sees the same state.
			Reopen: func() (coordination.Coordination, error) { return c, nil },
		}
	})
}

func TestSourceConformance(t *testing.T) {
	sourcestoretest.Run(t, func(*testing.T) sourcestoretest.Subject { return sourcestoretest.Subject{Store: NewSources()} })
}

// The tokens share the namespaces with the sources: deleting one takes its tokens.
func TestTokenConformance(t *testing.T) {
	tokenstoretest.Run(t, func(*testing.T) tokenstoretest.Subject {
		src := NewSources()
		return tokenstoretest.Subject{
			Store: NewTokens(src),
			Namespace: func(t *testing.T, owner shape.Added, name string) token.NamespaceRef {
				ns, err := src.FindOrCreateNamespace(owner, name)
				if err != nil {
					t.Fatal(err)
				}
				return token.NamespaceRef{ID: ns.ID, Name: ns.Name}
			},
			DeleteNamespace: func(t *testing.T, owner shape.Added, name string) {
				if err := src.DeleteNamespace(context.Background(), owner, name); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func TestTmpConformance(t *testing.T) {
	tmpstoretest.Run(t, func(*testing.T) tmpstoretest.Subject { return tmpstoretest.Subject{Store: NewTmp()} })
}

func TestCronConformance(t *testing.T) {
	cronstoretest.Run(t, func(*testing.T) cronstoretest.Subject {
		ns := NewSources()
		return cronstoretest.Subject{
			Store: NewCrons(ns),
			Namespace: func(t *testing.T, o shape.Added, name string) uint {
				n, err := ns.FindOrCreateNamespace(o, name)
				if err != nil {
					t.Fatal(err)
				}
				return n.ID
			},
			DeleteNamespace: func(t *testing.T, o shape.Added, name string) {
				if err := ns.DeleteNamespace(context.Background(), o, name); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func TestShardConformance(t *testing.T) {
	shardstoretest.Run(t, func(*testing.T) shardstoretest.Subject {
		next := uint(0)
		return shardstoretest.Subject{Store: NewShards(), Concurrent: true, Namespace: func(*testing.T) uint { next++; return next }}
	})
}
