# RunOS ClusterAgent Documentation

Welcome to the RunOS ClusterAgent documentation. This guide will help you understand what the ClusterAgent does and how it works within your Kubernetes cluster.

## What is ClusterAgent?

The RunOS ClusterAgent is a service that runs inside your Kubernetes cluster to help manage and automate various cluster operations. Think of it as a helper that takes care of important background tasks so your applications can run smoothly.

## Key Features

### Automated Certificate Management

The ClusterAgent handles the automatic provisioning and renewal of SSL/TLS certificates for your cluster's domain. It works with Let's Encrypt to obtain and maintain wildcard certificates, ensuring your applications are always accessible via secure HTTPS connections without manual intervention.

### Secure Cluster Operations

The agent performs privileged operations within your cluster when needed, such as managing secrets, viewing pods, and coordinating with the RunOS platform. All operations are authenticated and secured through mutual TLS encryption.

### Always Connected

The ClusterAgent maintains a persistent, secure connection to the RunOS platform. This connection allows the platform to:
- Monitor your cluster's health
- Coordinate certificate renewals
- Execute authorized management operations
- Provide real-time cluster status

## How It Works

The ClusterAgent runs as a single pod within your cluster's `runos` namespace. It requires elevated Kubernetes permissions to perform its duties, including:

- Managing certificates and secrets
- Reading pod information
- Coordinating with cert-manager for certificate issuance
- Maintaining secure communication with the RunOS platform

### Initial Installation and Bootstrap

The ClusterAgent is installed automatically when configuring your cluster for the first time. During the initial startup, the agent uses a unique bootstrap process to establish secure communication:

1. **Bootstrap Phase**: When the agent first starts, it temporarily uses the node's certificate to establish initial authentication with the RunOS platform
2. **mTLS Certificate Provisioning**: Once authenticated, the agent receives its own dedicated mutual TLS (mTLS) certificate from the platform
3. **Secure Operation**: After receiving its mTLS certificate, the agent switches to using its own credentials for all subsequent communication
4. **Automatic Renewal**: The agent's mTLS certificate is automatically renewed before expiration, ensuring uninterrupted secure communication

## Security

Security is a top priority for the ClusterAgent:

- **Mutual TLS Authentication**: All communication between the agent and the RunOS platform uses certificate-based authentication
- **Automatic Reconnection**: If the connection is interrupted, the agent automatically reconnects with exponential backoff
- **Secure Credential Storage**: All credentials are stored as Kubernetes secrets and never exposed in logs or configuration files
- **Regular Health Checks**: The agent performs heartbeat checks to ensure the connection remains healthy

## Next Steps

- [Certificate Management](./certificate-management.md) - Learn how automatic certificate management works
- [Cluster Operations](./cluster-operations.md) - Understand what operations the agent performs

## Support

For issues or questions about the RunOS ClusterAgent, please contact RunOS support or consult the main RunOS platform documentation.
