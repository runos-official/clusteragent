# Certificate Management

The RunOS ClusterAgent automates the management of SSL/TLS certificates for your cluster's domain, ensuring your applications are always accessible via secure HTTPS connections.

## Overview

The agent handles wildcard certificate provisioning and renewal for your cluster domain through integration with Let's Encrypt, a free and automated certificate authority. This means you never have to manually request, install, or renew certificates.

## How It Works

### Automatic Certificate Issuance

When your cluster needs a certificate:

1. **Certificate Request**: The cluster's cert-manager requests a new certificate from Let's Encrypt
2. **DNS Challenge**: Let's Encrypt sends a DNS-01 challenge to verify you control the domain
3. **Challenge Response**: The ClusterAgent receives the challenge and coordinates with the RunOS platform to update your DNS records
4. **Verification**: Let's Encrypt verifies the DNS record and issues the certificate
5. **Installation**: The certificate is automatically installed and made available to your applications

### Automatic Renewal

Certificates don't last forever - they typically expire after 90 days. The ClusterAgent ensures your certificates are always valid by:

- Monitoring certificate expiration dates
- Automatically renewing certificates before they expire (typically 30 days before expiration)
- Seamlessly updating your applications with renewed certificates
- No downtime or manual intervention required

## Wildcard Certificates

The ClusterAgent manages wildcard certificates for your cluster domain. A wildcard certificate covers:

- Your main domain (e.g., `example.com`)
- All subdomains (e.g., `app.example.com`, `api.example.com`)

This means you can deploy as many applications as you need without requesting individual certificates for each subdomain.

## What You See

From your perspective, certificate management is completely transparent:

- Your applications are automatically secured with HTTPS
- Certificates renew before expiration
- No configuration or maintenance required
- No service interruptions during renewal

## DNS-01 Challenge

The agent uses the DNS-01 challenge method for certificate validation. This method:

- Proves domain ownership by creating a specific DNS record
- Works for wildcard certificates (unlike HTTP-01 challenges)
- Doesn't require your applications to be publicly accessible during validation
- Is fully automated by the ClusterAgent

## Troubleshooting

### Certificate Not Issued

If a certificate isn't being issued automatically:

- Verify the ClusterAgent pod is running in the `runos` namespace
- Check that your cluster has connectivity to the RunOS platform
- Ensure cert-manager is properly installed in your cluster

### Certificate Renewal Issues

Certificates typically renew automatically. If you notice renewal issues:

- Check the ClusterAgent logs for error messages
- Verify the agent can communicate with the RunOS platform
- Ensure your DNS is properly configured

## Security Considerations

- **Private Keys**: Certificate private keys are stored securely as Kubernetes secrets and never leave your cluster
- **Validation**: Only authorized agents can complete DNS challenges for your domain
- **Encryption**: All communication between the agent and RunOS platform is encrypted
- **No Downtime**: Certificate renewals happen seamlessly without service interruption

## Technical Details

For those interested in the technical implementation:

- The agent implements a cert-manager DNS-01 webhook solver
- Integrates with the ACME protocol used by Let's Encrypt
- Uses the `acme.runos.com` API group for certificate challenges
- Operates on port 443 within the cluster for webhook callbacks
