package datastore

// BuildArg is a single effective Docker build argument applied to a build,
// tagged with the source it was resolved from ("yaml" | "cli"). Conductor
// merges the runos.yaml buildArgs map with the CLI --build-arg flags, applies
// precedence and validation, and sends the already-resolved list; the cluster
// agent persists it verbatim and forwards each entry to BuildKit. Values are
// plaintext (secret-sourced args are out of scope for this objective).
type BuildArg struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// InsertBuildKitJobArgs writes one row per effective build arg for a build
// job, keyed by build_job_id (the buildkit_jobs.job_id of the owning build).
//
// A nil/empty slice is a no-op: deploys with no build args write zero rows,
// exactly as before this feature existed. Callers pass the same list they
// hand to BuildKit, so the persisted record reflects what actually produced
// the image. Rows are created in slice order so the autoincrement id preserves
// insertion order for readers.
func InsertBuildKitJobArgs(buildJobID string, args []BuildArg) error {
	if len(args) == 0 {
		return nil
	}
	gdb, err := activeDB()
	if err != nil {
		return err
	}
	rows := make([]BuildKitJobArgModel, 0, len(args))
	for _, a := range args {
		rows = append(rows, BuildKitJobArgModel{
			BuildJobID: buildJobID,
			ArgKey:     a.Key,
			ArgValue:   a.Value,
			Source:     a.Source,
		})
	}
	// CreateInBatches preserves slice order, so the autoincrement id keeps the
	// caller's insertion order, matching the row-by-row INSERT it replaces.
	return gdb.CreateInBatches(rows, len(rows)).Error
}
