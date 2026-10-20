# Security policy

Disk Recovery Lab runs with administrator rights and reads raw disks, so security reports are taken seriously.

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private vulnerability reporting instead (repository → **Security**
→ **Report a vulnerability**). Include the version (`--version`), your OS, and steps to reproduce. You will get an
answer within a week.

## Design notes for reviewers

- The server listens on `127.0.0.1` only, on a random port.
- Every launch creates a random 192-bit token. It is passed once in the window URL, then kept in an HttpOnly,
  SameSite=Strict cookie. Requests without it are refused.
- Requests whose `Host` header is not the server's own address are refused, which blocks DNS-rebinding attacks.
- The API only accepts JSON `POST`s, so cross-site form posts are rejected.
- The token reaches the elevated process through a private temporary file, never on the command line.
- File previews are served with `X-Content-Type-Options: nosniff` and a sandboxing CSP.
- Sources are opened read-only. Recovered files cannot be written to the drive being recovered.
- The elevated server exits after 10 minutes without a window, unless a scan or recovery is running.
