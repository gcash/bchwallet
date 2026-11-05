package bcmr

import "github.com/gcash/bchlog"

// log is the package's logger, disabled until UseLogger gives it one.
var log = bchlog.Disabled

// UseLogger sets the package's logger.
func UseLogger(logger bchlog.Logger) {
	log = logger
}
