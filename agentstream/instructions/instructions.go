// Package instructions implements the cluster agent's instruction handlers:
// the units of work the control plane dispatches over the agent stream (app
// builds, image-exists checks, SQL query/schema, cert and DNS01 management,
// CLI deploy/pull tokens, pod/ingress/cert listing, one-shot jobs, secret-file
// writes, and web-request proxying). Map routes each instruction type to its
// handler.
package instructions

// Map of message types to handler functions
// func(msgJsonB64=")  (replyType, replyMsgJsonB64, err)
var Map = map[string]func(string) (string, string, error){
	"WEB_REQUEST":             WebRequestFromServer,
	"WEB_REQUEST_FOLLOW":      WebRequestFollowFromServer,
	"POD_VIEWER":              K8sPodStatsRequestFromServer,
	"PUT_SECRET_FILE":         PutSecretFile,
	"LIST_BUILD_JOBS":         ListBuildJobs,
	"LIST_BUILD_LOGS":         ListBuildLogs,
	"CREATE_CLI_DEPLOY_TOKEN": CreateCLIDeployToken,
	"CREATE_CLI_PULL_TOKEN":   CreateCLIPullToken,
	"LIST_CLI_ARCHIVES":       ListCLIArchives,
	"VCS_FETCH_SOURCE":        VcsFetchSource,
	"VCS_BUILD":               VcsBuild,
	"HARBOR_IMAGE_EXISTS":     HarborImageExists,
	"SQL_SCHEMA":              SQLSchema,
	"SQL_QUERY":               SQLQuery,
	"LIST_INGRESSES":          ListIngresses,
	"LIST_CERTIFICATES":       ListCertificates,
	"RUN_ONESHOT_JOB":         RunOneShotJob,
	"ONESHOT_JOB_STATUS":      OneShotJobStatus,
	"CLEANUP_ONESHOT_JOB":     CleanupOneShotJob,
}
