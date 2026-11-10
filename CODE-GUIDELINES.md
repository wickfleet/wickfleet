# Code guidelines

| Rule | Why |
|---|---|
| Standard library first; a new dependency needs a reason in the PR | small attack surface, easy audit |
| Config by env var or file, never by flag-only or baked in | same image runs in compose and Kubernetes |
| One process per container, a `/healthz`, state only on declared volumes | the deployer can treat every part the same |
| No hostnames, user names or domains of a real deployment in code or tests | the repo is meant to go public; use `example.org`, `alice`, `bob` |
| Secrets never in logs, errors, metrics or test output | errors that wrap a URL with a token are rewritten, see `notify.Telegram` |
| A failing bot is reported as failing, never as zero | a guessed number is worse than a gap |
| Dry and live money never in one sum | they are different things |
| Every policy branch in `internal/authz` has a test case | it is the security boundary |
| A file another goroutine or process reads is replaced whole (`atomicfile.Write`), never rewritten in place | an in-place write shows readers an empty or torn file |
| Comments: two lines at most, the why goes in the commit | |
| Docs: tables over prose, no em dashes, state what is true now | |

Layout: `cmd/` for binaries, `internal/<area>` per concern, tests next to the
code. `make fmt` before committing.
