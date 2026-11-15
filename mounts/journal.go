package mounts

import (
	weftsdk "github.com/w3ft-net/weft-go"
	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/warning"
)

// A Config with a History records every write to a tracked file (/bin scripts,
// /cron.d entries, /etc/dashboards and /etc/notify.yaml) there, after the live
// state has taken it. The live state is the record of what is current, so a
// history that can't take a change is logged and the save stands; a Config
// without a History (the hosted build, whose history is its version rows) does
// nothing here.

// identity is who the Config's user is, for attribution.
func (c Config) identity() history.Identity {
	if c.UserID == "" {
		return history.Identity{}
	}
	if c.Identity != nil {
		return c.Identity(c.UserID)
	}
	return history.Identity{Name: c.UserID}
}

// wrote reports whether a write came back without a blocking error: a save
// that only carries warnings still happened.
func wrote(err error) bool {
	blocking, _ := warning.Split(err)
	return len(blocking) == 0
}

// journalWrite records the file at path as it reads now, if the write that
// returned err happened.
func (c Config) journalWrite(path string, err error, read func() ([]byte, error)) {
	if c.History == nil || !wrote(err) {
		return
	}
	data, rerr := read()
	if rerr != nil {
		weftsdk.Errorf(c.ctx(), "history: reading back %s to record it: %v (the save stands)", path, rerr)
		return
	}
	c.record(history.Change{Path: path, Content: string(data), Message: history.MessageFrom(c.ctx())})
}

// journalDelete records the removal of path, if the removal that returned err
// happened.
func (c Config) journalDelete(path string, err error) {
	if c.History == nil || !wrote(err) {
		return
	}
	c.record(history.Change{Path: path, Deleted: true})
}

func (c Config) record(ch history.Change) {
	ch.Author = c.identity()
	if _, err := c.History.Record(c.ctx(), c.partition(), ch); err != nil {
		weftsdk.Errorf(c.ctx(), "history: recording %s: %v (the save stands)", ch.Path, err)
	}
}

// RewordHistory sets the message of the newest commit of path in the history.
// It does nothing for a Config without one. A commit that has been pushed
// refuses (history.ErrPublished).
func (c Config) RewordHistory(path, message string) error {
	if c.History == nil {
		return nil
	}
	id, err := c.History.Latest(c.partition(), path)
	if err != nil {
		return err
	}
	return c.History.Reword(c.ctx(), c.partition(), id, message)
}
