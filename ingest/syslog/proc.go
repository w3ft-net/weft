package syslog

import (
	"strconv"
	"strings"
)

// SplitProcID separates a platform PROCID into a sub-app base name and
// a per-instance label:
//
//	"web.1"              → ("web", "web.1")
//	"router"             → ("router", "router")
//	"scheduler.1234abcd" → ("scheduler.1234abcd", "scheduler.1234abcd")
//	"-" / ""             → ("", "")
//
// A trailing numeric segment is treated as an instance ordinal and
// stripped from the base, so every instance of one proc type lands in
// the same pseudofile while the ordinal stays on the instance label for
// cross-instance rollups. A non-numeric trailing segment is kept intact
// (some add-ons carry a dot in the proc name itself).
func SplitProcID(procid string) (base, instance string) {
	if procid == "" || procid == "-" {
		return "", ""
	}
	if i := strings.LastIndexByte(procid, '.'); i > 0 {
		if _, err := strconv.Atoi(procid[i+1:]); err == nil {
			return procid[:i], procid
		}
	}
	return procid, procid
}
