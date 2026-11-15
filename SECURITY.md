# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public
issue or pull request for a security problem.

Include the affected version or commit, how to reproduce it, and the impact
you expect. You should get a first answer within a week.

In scope, for example: reading or deleting another user's jobs, bypassing
token authentication or the upload/duration limits, crashes caused by crafted
WAV input, and reaching files outside the data directory.

## Supported versions

Only the latest commit on `main` receives fixes.

## Deployment notes

- Store only token hashes in `tokens.json`; never commit tokens or `tokens.json`.
- Serve the API over HTTPS (built-in TLS or a reverse proxy) when it leaves
  the local machine.
- Keep the systemd resource limits so a long job cannot starve the host.
