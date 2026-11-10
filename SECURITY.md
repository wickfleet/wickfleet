# Security policy

## Reporting

Report security issues privately, never in a public issue. Email
**security@wickfleet.com** with a description, steps to reproduce and the commit or
image tag. Acknowledgement within 5 business days.

Disclosure is coordinated: a fix ships before details go public, normally
within 90 days.

## Scope

| In | Out |
|---|---|
| `/authz`: roles, ownership, method rules, key injection | Freqtrade itself, report upstream |
| the built-in login: sessions, setup token, rate limit, header stripping | |
| leaking a bot key, an IdP token or the Telegram token | the reverse proxy, oauth2-proxy and the IdP |
| the webhook endpoint accepting outside posts | host OS and container runtime |
| totals that mix dry and live money | |
| a Docker request from the hub that `wickfleet-dockerguard` lets through but should not | |

## Design rules that matter for security

- Bot API keys never reach the browser. The hub adds them per request after the
  role check.
- With `WICKFLEET_AUTH=proxy`, identity comes only from oauth2-proxy's
  `X-Auth-Request-*` headers, which Traefik overwrites with the auth response,
  so a client cannot set them.
- Those headers count only next to `X-Wickfleet-Authz-Key`, which Traefik adds
  and strips again; a container on the internal network cannot forge one.
- With `WICKFLEET_AUTH=builtin`, the hub drops every client `X-Auth-Request-*`
  header and sets them only from its own session. Passwords are argon2id
  hashes in a 0600 file, there is no default user, and the first admin needs a
  one-time token printed inside the container.
- The hub never talks to Docker directly. `wickfleet-dockerguard` sits between
  the hub and the socket proxy and allows only the hub's own calls: a create
  must use a pinned image, the bot user, the fleet network, `CapDrop ALL`,
  `no-new-privileges` and binds under the fleet's data dirs; anything else, any
  unknown field and any other endpoint is refused. A hub bug cannot start a
  privileged container or mount the host.
- Registry writes use a deploy key scoped to the one registry repo, never a
  user token; every write needs `X-Wickfleet-Request` against CSRF.
- Exchange keys never sit on a shared instance.
- Live trading is off unless the instance sets `WICKFLEET_LIVE_TRADING=true`.
  Off, a `dry_run: false` bot is refused three times over: at the API, at
  registry load and when its freqtrade config renders. On, exchange keys come
  only from `WICKFLEET_EXCHANGE_KEYS_DIR/<bot>.json` (owner-only, no symlink),
  set after `config_extra` is merged, so neither the registry, the UI nor
  `config_extra` can put a key into a bot. Keys never appear in the API, and
  key-file errors never carry the file's content.
