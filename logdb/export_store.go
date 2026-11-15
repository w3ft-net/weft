package logdb

import "errors"

var errNoExportStore = errors.New("logdb: no shard store to persist export facts")

func (d ShardDeps) exportStore() (ExportStore, error) {
	if d.Store == nil {
		return nil, errNoExportStore
	}
	return d.Store, nil
}
