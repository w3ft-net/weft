package mem

import "github.com/w3ft-net/weft/notifycfg"

// Stores is every domain's in-memory store, sharing the namespaces the sources
// store holds (tokens and crons refer to them).
type Stores struct {
	Sources    *Sources
	Crons      *Crons
	Bins       *Bins
	Notices    *Notices
	Tokens     *Tokens
	Tmp        *Tmp
	Ingest     *IngestFailures
	Coord      *Coordination
	Shards     *Shards
	DLQ        *DLQ
	Login      *Login
	Dashboards *Dashboards
	// NotifyStore is the notification config and its secrets; Notify is the
	// service over it.
	NotifyStore *Notify
	Notify      notifycfg.Service
}

// Open is a set of empty stores.
func Open() *Stores {
	src := NewSources()
	n := NewNotify()
	return &Stores{
		Sources:     src,
		Crons:       NewCrons(src),
		Bins:        NewBins(),
		Notices:     NewNotices(),
		Tokens:      NewTokens(src),
		Tmp:         NewTmp(),
		Ingest:      NewIngestFailures(),
		Coord:       NewCoordination(),
		Shards:      NewShards(),
		DLQ:         NewDLQ(),
		Login:       NewLogin(),
		Dashboards:  NewDashboards(),
		NotifyStore: n,
		Notify:      n.Service(),
	}
}
