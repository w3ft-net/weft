package sqlite

import (
	"database/sql"

	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/notifycfg"
)

// Stores is every domain's SQLite store over one database.
type Stores struct {
	Sources *SourceBackend
	Crons   *CronBackend
	Bins    *BinBackend
	Notices *NoticeBackend
	Tokens  *TokenBackend
	Tmp     *TmpBackend
	Ingest  *IngestFailBackend
	Coord   *CoordinationBackend
	Shards  *ShardBackend
	DLQ     *mem.DLQ
	Login   *LoginBackend
	// Dashboards is /etc/dashboards.
	Dashboards *DashboardBackend
	// Notify is the notification config, its aliases and secrets both kept by
	// the one store.
	Notify notifycfg.Service
}

// Open makes each store's tables in db, which they share.
func Open(db *sql.DB) (*Stores, error) {
	var (
		s   Stores
		err error
	)
	if s.Sources, err = NewSourceBackend(db); err != nil {
		return nil, err
	}
	if s.Crons, err = NewCronBackend(db); err != nil {
		return nil, err
	}
	if s.Bins, err = NewBinBackend(db); err != nil {
		return nil, err
	}
	if s.Notices, err = NewNoticeBackend(db); err != nil {
		return nil, err
	}
	if s.Tokens, err = NewTokenBackend(db); err != nil {
		return nil, err
	}
	if s.Tmp, err = NewTmpBackend(db); err != nil {
		return nil, err
	}
	if s.Ingest, err = NewIngestFailBackend(db); err != nil {
		return nil, err
	}
	if s.Coord, err = NewCoordinationBackend(db); err != nil {
		return nil, err
	}
	if s.Shards, err = NewShardBackend(db); err != nil {
		return nil, err
	}
	s.DLQ = mem.NewDLQ()
	if s.Login, err = NewLoginBackend(db); err != nil {
		return nil, err
	}
	if s.Dashboards, err = NewDashboardBackend(db); err != nil {
		return nil, err
	}
	notify, err := NewNotifyBackend(db)
	if err != nil {
		return nil, err
	}
	s.Notify = notifycfg.Service{Aliases: notify, Secrets: notify}
	return &s, nil
}
