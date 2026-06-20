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
// the image.
func InsertBuildKitJobArgs(buildJobID string, args []BuildArg) error {
	if len(args) == 0 {
		return nil
	}
	query := `
		INSERT INTO buildkit_job_args (build_job_id, arg_key, arg_value, source)
		VALUES (?, ?, ?, ?)
	`
	for _, a := range args {
		if _, err := db.Exec(query, buildJobID, a.Key, a.Value, a.Source); err != nil {
			return err
		}
	}
	return nil
}
