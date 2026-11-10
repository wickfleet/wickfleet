# Contributing

## Before a change

- Open or claim an issue first and say what you will do. Unclaimed small bug
  fixes are fine as a drive-by.
- Read [CODE-GUIDELINES.md](CODE-GUIDELINES.md).

## Workflow

| | |
|---|---|
| Branch | off `main`, one topic per branch |
| Commits | conventional prefix (`feat:`, `fix:`, `docs:`, `chore:`), subject 50 chars or less, lowercase |
| Sign-off | every commit carries `Signed-off-by` (DCO): `git commit -s` |
| Gate | `make setup lint test vuln` green locally |
| Tests | a behaviour change comes with a test that fails without it |

The sign-off is your statement under the
[Developer Certificate of Origin](https://developercertificate.org/) that you
wrote the change or may submit it under the project's licence. A pull request
with an unsigned commit is not merged. There is no CLA: you keep the copyright
of your change, and it is licensed GPL-3.0 like the rest.

## Where the code lives

Development happens in a private tree. This repository receives one snapshot
commit per release (`release: <date>`, with a `Source-Commit` trailer naming
the private commit), opened as a `publish/<branch>-<date>` pull request.

| Step | What happens |
|---|---|
| Export | only the paths on an allowlist leave the private tree; the rest never does |
| Leak check | the export is scanned for internal names and for secrets (gitleaks); any hit stops the publish |
| Build check | the export alone must pass `make setup build lint test` |
| Snapshot | one commit on a `publish/` branch, merged by the maintainer |

A pull request here is reviewed as usual. Once accepted, the change is carried
into the private tree with your authorship and sign-off, and comes back in the
next snapshot.

## Checks

| Target | Checks |
|---|---|
| `make lint` | gofmt, `go vet`, `go mod tidy -diff` |
| `make test` | `go test -race` with coverage |
| `make vuln` | govulncheck on reachable code |
