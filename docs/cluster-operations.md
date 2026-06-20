# Cluster Operations

The RunOS ClusterAgent performs various operations within your Kubernetes cluster to support the RunOS platform and keep your applications running smoothly.

## Overview

The ClusterAgent acts as a bridge between your Kubernetes cluster and the RunOS platform, executing authorized operations on behalf of the platform when needed.

## Types of Operations

### Monitoring and Health Checks

The agent continuously monitors the health of the connection to the RunOS platform:

- **Heartbeat**: Sends regular heartbeat messages to confirm the connection is active
- **Automatic Reconnection**: If the connection is lost, the agent automatically reconnects
- **Status Reporting**: Provides cluster status information to the platform

### Secret Management

The agent can manage Kubernetes secrets within your cluster:

- Updating configuration secrets when needed
- Managing secure credentials for platform integration
- Ensuring sensitive data is properly stored and protected

### Pod Information

The agent can retrieve information about pods running in your cluster:

- Viewing pod status and health
- Gathering deployment information
- Providing visibility into your cluster's workloads

### Certificate Operations

As described in the [Certificate Management](./certificate-management.md) documentation, the agent coordinates certificate issuance and renewal with cert-manager and Let's Encrypt.

## Security and Permissions

### Required Permissions

The ClusterAgent requires elevated permissions within your cluster to perform its duties. These permissions are scoped to the `runos` namespace and specific cluster-wide operations needed for certificate management.

The agent has access to:

- Read and write secrets in the `runos` namespace
- View pod information
- Interact with cert-manager for certificate operations
- Manage certificate-related resources

### Authentication

All operations are authenticated and authorized:

- **Mutual TLS**: The agent authenticates to the RunOS platform using certificates
- **Service Account**: Uses a Kubernetes service account with defined permissions
- **Audit Trail**: All operations can be traced through Kubernetes audit logs

### What the Agent Cannot Do

To maintain security, the agent:

- Cannot modify resources outside the `runos` namespace without explicit authorization
- Cannot access application data or user information
- Cannot make changes to cluster infrastructure
- Only executes operations explicitly authorized by the RunOS platform

## Communication

### Persistent Connection

The agent maintains a persistent bidirectional connection with the RunOS platform:

- **Encrypted**: All communication uses TLS encryption
- **Bidirectional**: Allows both the agent to send information and the platform to send instructions
- **Resilient**: Automatically reconnects if the connection is interrupted
- **Low Overhead**: Uses efficient binary protocol (gRPC) to minimize bandwidth

### Reconnection Behavior

If the connection to the RunOS platform is interrupted:

1. The agent detects the disconnection
2. Waits with exponential backoff (starting at 1 second, max 60 seconds)
3. Attempts to reconnect automatically
4. Resumes normal operations once reconnected
5. After 10 failed attempts, the agent will require a restart

## Resource Usage

The ClusterAgent is designed to be lightweight:

- **CPU**: Typically uses 100m CPU (with a limit of 200m)
- **Memory**: Uses around 128Mi RAM (with a limit of 256Mi)
- **Network**: Minimal bandwidth for heartbeats and occasional operations
- **Single Instance**: Only one agent pod runs per cluster

## Monitoring the Agent

### Checking Agent Status

You can check if the agent is running properly:

```bash
kubectl get pods -n runos
```

Look for the `runos-cluster-agent` pod with status "Running".

### Viewing Agent Logs

To see what the agent is doing:

```bash
kubectl logs -n runos deployment/runos-cluster-agent
```

Normal operation shows:
- Successful connection to RunOS servers
- Regular heartbeat responses
- Certificate challenge handling (when certificates are being issued/renewed)

### Health Checks

The agent exposes health check endpoints:
- **Liveness**: Confirms the agent is running
- **Readiness**: Confirms the agent is ready to accept requests

Kubernetes automatically restarts the agent if health checks fail.

## Troubleshooting

### Agent Not Starting

If the agent pod isn't starting:
- Verify the `runos` namespace exists
- Check that required secrets are present
- Review pod logs for error messages

### Connection Issues

If the agent can't connect to the RunOS platform:
- Verify your cluster has internet connectivity
- Check firewall rules allow outbound HTTPS connections
- Ensure DNS resolution is working

### High Resource Usage

The agent should have minimal resource usage. If you notice high usage:
- Check the logs for errors or excessive reconnection attempts
- Verify the connection to the RunOS platform is stable
- Contact RunOS support for assistance

## Updates

The ClusterAgent is automatically updated by the RunOS platform:
- Updates are applied during maintenance windows
- The agent gracefully restarts to apply updates
- No manual intervention required
- Minimal or no downtime during updates

## Best Practices

- **Don't modify** the agent deployment or service account permissions
- **Monitor** agent logs periodically to ensure normal operation
- **Maintain** network connectivity between your cluster and the RunOS platform
- **Keep** cert-manager installed and properly configured
- **Report** any unusual behavior or errors to RunOS support
