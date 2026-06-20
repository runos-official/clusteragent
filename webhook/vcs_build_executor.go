package webhook

import (
	"context"
	"log"

	"github.com/runos-official/clusteragent/agentstream"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/datastore"
)

// RunVcsBuild is the implementation of instructions.VcsBuildExecutor. It
// runs the BuildKit + Harbor + kubectl tail of a VCS deploy against a
// workdir that VCS_FETCH_SOURCE already populated.
//
// Wired in main.go — keeping the executor here (rather than in the
// instructions package) means the instructions package doesn't have to
// import agentstream, which would create a cycle through agent.go.
func RunVcsBuild(input instructions.VcsBuildExecutorInput) {
	defer input.Cleanup()

	ctx := context.Background()

	k8sClient, err := agentstream.NewK8sClient()
	if err != nil {
		log.Printf("VCS_BUILD executor: failed to create k8s client: %v", err)
		datastore.UpdateBuildKitJobStatus(input.JobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(input.JobID, "ERROR: could not initialise k8s client")
		return
	}

	buildkitConfig, err := k8sClient.GetBuildKitConfig(ctx)
	if err != nil {
		log.Printf("VCS_BUILD executor: failed to read buildkit config: %v", err)
		datastore.UpdateBuildKitJobStatus(input.JobID, datastore.JobStatusFailed)
		datastore.InsertBuildKitLog(input.JobID, "ERROR: could not read buildkit config")
		return
	}

	ProcessBuildAndDeploy(ctx, k8sClient, BuildAndDeployInput{
		Workdir:            input.Workdir,
		OSID:               input.OSID,
		JobID:              input.JobID,
		Tag:                input.SHA,
		ContextPath:        input.ContextPath,
		DockerfileDir:      input.DockerfileDir,
		DockerfileFilename: input.DockerfileFilename,
		BuildArgs:          input.BuildArgs,
		BuildOnly:          input.BuildOnly,
		Repo:               input.Repo,
		Branch:             input.Branch,
	}, buildkitConfig)
}
