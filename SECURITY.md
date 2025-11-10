# Security Policy

The POS Service processes payment-related and operational data. Please follow these guidelines to protect merchants and customers.

## Supported Versions

| Version | Supported |
|---------|-----------|
| `main` | ✅ |
| Latest tagged release (future) | ✅ |
| Older branches | ❌ |

## Reporting Vulnerabilities

1. Email `security@bengobox.com` with details (do not open public issues).
2. Provide reproduction steps, impact analysis, and suggested mitigations.
3. We acknowledge within 48 hours and coordinate remediation + disclosure.

## Secure Development Practices

- Never store plaintext card data; delegate payments to treasury integrations.
- Protect device registration and session APIs with proper authentication (auth-service tokens).
- Validate POS input rigorously to prevent tampering or inventory fraud.
- Log all voids, discounts, cash drawer events with correlation IDs for audits.
- Run security linters and `govulncheck` in CI/CD.

## Operational Controls

- Enable TLS/mTLS for all service communication.
- Rotate API keys and secrets regularly; store them in a managed secrets manager.
- Monitor audit logs for suspicious activity (void abusers, cash variances, sync failures).
- Apply timely patch management to OS, runtime, and dependencies.

Thank you for helping keep the POS platform secure.

