package agent

import "github.com/Autumn-27/artex/db"

// coverageOn reports whether asset-coverage tools stay available for this task.
// A nil store is the historical default (enabled). A database error is logged and
// treated as enabled so a transient read failure does not drop the tools mid-run.
func coverageOn(as *db.AssetStore, taskID int64) bool {
	if as == nil {
		return true
	}
	return as.CoverageEnabled(taskID)
}
