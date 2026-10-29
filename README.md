# golang-sole

Sneaker raffles with a draw anyone can check, written in **Go** and run on **Kubernetes**. It is the Go and Kubernetes
version of [dotnet-sole](https://github.com/joshbarros/dotnet-sole): the same saga, the same HTTP API and the same Angular
UI, so the browser specs and the smoke script of the .NET system run against this one unchanged.

Entering a raffle takes no stock: it only records one entry per person. At the draw time a drawer in the raffles module
(every API replica runs one) hands the pairs out, and the order it uses was **committed before the first entry existed**.
When the raffle opens, the server picks a random seed and publishes only its SHA-256 (`seedHash`). At the draw the entries
are ordered by `SHA-256("seed:orderId")` ascending and the pairs are given out first fit; the seed is then revealed, and
anyone can hash it, compare it with the commitment, and recompute every rank (`GET /api/raffles/{code}/draw`). The draw runs
in one transaction behind a conditional `UPDATE` that flips the raffle from `Open` to `Drawn`, so several replicas drawing at
once draw it exactly once, and it locks the entries it draws (`FOR UPDATE`), so a withdrawal racing the draw either lands
first and the entry is not drawn, or lands after and releases the pairs it won: every pair is on the shelf or held, once.
Winners get a hold and go through the saga; the others are told "Not drawn: place N of M". A reference that contains the
poison token `POISON` is committed as rejected by the claims module, the message is dead-lettered, and a compensating command
gives the pairs back. A later refund does the same, but it does not roll back the claim that was already issued.

What is different here is how it runs. **Kubernetes is what runs the stack**: Postgres, Kafka, RabbitMQ, **two API replicas**
and two web replicas, started in a local [kind](https://kind.sigs.k8s.io) cluster with Kustomize manifests. Nothing is faked
for the demo: the same pipeline (`make cluster-up`, the smoke script, the browser specs, a script that kills pods mid-raffle)
runs in CI on a fresh kind cluster.

![The Sole UI signed in as the administrator, after a draw: the committed seed hash and the revealed seed side by side, one entry sold and two not drawn](docs/screenshot.png)

*The UI reads everything from the API; shown signed in as the seeded administrator on the cluster, after a draw: the committed
hash and the revealed seed are shown side by side, the winner of the single pair was sold, and the other entrants were told
their place in the order.*

## Order statuses

```mermaid
flowchart LR
    Requested -->|one entry recorded, nothing taken| Entered
    Requested -->|raffle closed, or already entered: no entry written| Rejected
    Entered -->|drawn: pairs held| Held
    Entered -->|drawn: not picked, no hold written| Rejected
    Entered -->|withdrawn before the draw| Refunded
    Held -->|claim issued| Sold
    Held -->|poison reference: claim rejected| Rejected
    Held -->|refund| Refunded
    Sold -->|refund| Refunded
```

`Refunded` and `Rejected` (after a hold) put the pairs back through a `ReleasePairs` command, and the raffles module
releases a hold at most once. Withdrawing an entry before the draw just deletes it; after the draw it releases the pairs the
entry won, once, and a losing entry has nothing to release. The claim has its own status (`Issued` or `Rejected`) and a refund
never changes it. If the refund gets there before the final mark, the late mark finds the order is no longer `Held` and does
nothing.

## How the modules talk

```mermaid
sequenceDiagram
    participant C as Client
    participant O as Orders
    participant R as Raffles
    participant L as Claims

    C->>O: POST /api/orders
    Note over O: order + outbox row, one transaction
    O-->>C: 202 Requested
    O->>R: pairs.reserve (relay → RabbitMQ, copy to Kafka)
    alt the raffle is open and this is the person's first entry
        Note over R: inbox claim, INSERT ... ON CONFLICT DO NOTHING on (raffle, person), outbox row
        R->>O: pairs.entered (Requested → Entered): no pairs are taken yet
    else closed, or already entered
        R->>O: pairs.short (Requested → Rejected, reason Closed or Already entered)
    end
    Note over R: at the draw time, every replica's drawer tries to claim the raffle (Open → Drawn); one wins
    Note over R: lock the entries, order by SHA-256(seed:orderId), hand out pairs first fit, reveal the seed
    alt the entry won
        R->>L: pairs.held
        R->>O: pairs.held (Entered → Held)
        alt reference is clean
            Note over L: claim, outbox row
            L->>O: claim.issued (Held → Sold)
        else reference contains POISON
            Note over L: committed Rejected, then the message is dead-lettered
            L->>R: pairs.release (compensating command)
            L->>O: claim.rejected (Held → Rejected)
            R->>O: pairs.released
        end
    else the entry was not picked
        R->>O: pairs.short (Entered → Rejected, no hold, "Not drawn: place N of M")
    end
    C->>O: POST /api/orders/{id}/refund
    O->>R: pairs.release (Entered, Held or Sold → Refunded)
    R->>O: pairs.released
```

| Module | Owns | Consumes | Emits |
| --- | --- | --- | --- |
| **identity** (`internal/identity`) | `identity` schema: users, roles, refresh tokens, rate-limit counters, the dev mailbox. | | |
| **orders** (`internal/orders`) | `orders.orders`: status, pairs, total, entrant reference. Validation, the total, the poison rule's order side. | `pairs.entered`, `pairs.held`, `pairs.short`, `claim.issued`, `claim.rejected`, `pairs.released` | `pairs.reserve`, `pairs.release` (refund) |
| **raffles** (`internal/raffles`) | `raffles.raffles.pairs_left` (sole writer after the raffle opens), the committed seed, `raffle_entries` (one per person per raffle, with the outcome and rank the draw wrote) and `pair_holds`. | `pairs.reserve`, `pairs.release` (prefetch 1) | `pairs.entered`, `pairs.held`, `pairs.short`, `pairs.released` |
| **claims** (`internal/claims`) | `claims.claims`. | `pairs.held` | `claim.issued`, `claim.rejected`, `pairs.release` (poison) |
| **platform** (`internal/platform`) | The relay, the consumer loop with retry and dead-letter, the Kafka log, problem+json and the middleware, config, migrations. Each module owns its own `outbox_messages` and `inbox_messages` tables. | | |

RabbitMQ topology is declared in code (`internal/platform/messaging/rabbit.go`): exchange `sole.work`, one queue per module
(`raffles.commands`, `claims.work`, `orders.events`), a `.retry` queue per module (2 s TTL, then back to the main queue, 3
attempts), and a `.dlq` per module behind `sole.dlx`. A transient failure retries; a poison message goes straight to the
dead-letter queue after its rejection is committed. After a trip through the retry queue the broker's routing key is the queue
name, so the relay stores the original key in the message `type` and the consumer dispatches on that. Kafka has one topic,
`sole.events`, one partition, replication factor 1, keyed by order id.

## Architecture

One binary (`cmd/api`), four modules in `internal/`. Each module has its own Postgres **schema** (`identity`, `orders`,
`raffles`, `claims`), its own embedded SQL migrations and its own outbox and inbox tables. No module imports another; the one
cross-module read (a raffle's existence, price and whether it is still open) goes through a small interface that the orders
module declares and the raffles module satisfies, and `internal/app` is the only package that knows all of them.

```mermaid
flowchart TB
    main["cmd/api: serve | migrate"] --> app["internal/app: wiring, middleware chain, workers"]
    app --> identity & orders & raffles & claims
    identity["internal/identity: users, roles, JWT, refresh tokens, two-factor"]
    orders["internal/orders: schema orders"]
    raffles["internal/raffles: schema raffles, the draw"]
    claims["internal/claims: schema claims"]
    identity & orders & raffles & claims --> platform["internal/platform: httpx, messaging, pgdb, config"]
    orders -. "Catalog.Find" .-> raffles
```

Modules still talk only through messages: RabbitMQ carries the commands, Kafka keeps the event log, and the outbox and inbox
make delivery at-least-once and consumption idempotent. The dependencies are small on purpose: `pgx`, `amqp091-go`,
`franz-go`, `golang-jwt` and `x/crypto`; the HTTP layer is the standard library's `net/http` mux.

## On Kubernetes

```mermaid
flowchart LR
    host["127.0.0.1:8108 · :5228 · :15718"] -->|NodePort| web & api & mgmt
    subgraph ns["namespace sole (kind: one node)"]
        web["web ×2: nginx + Angular"] -->|/api| api
        api["api ×2: Go (init container: migrate)"] --> pg[("postgres StatefulSet")]
        api --> kafka[("kafka StatefulSet (KRaft)")]
        api --> rmq[("rabbitmq StatefulSet")]
        mgmt["RabbitMQ management"] --- rmq
    end
```

| What | How it is set up |
| --- | --- |
| API | A Deployment of **2 replicas**, `maxUnavailable: 0`, a PodDisruptionBudget (`minAvailable: 1`), spread over nodes. Nothing in it assumes a single instance: the relay claims outbox rows with `SKIP LOCKED`, the drawer claims a raffle with a conditional `UPDATE`, the auth rate limit and the session version live in Postgres. |
| Migrations | An **init container** runs `api migrate` before any replica serves (each module migrates behind a Postgres advisory lock, so several pods starting together are safe) and the API runs with `MIGRATE_ON_START=false`. |
| Probes | `startupProbe` and `livenessProbe` on `/api/health`; `readinessProbe` on `/api/health/ready`, which checks that every module's schema is there. |
| Data stores | One StatefulSet each for Postgres 17, Kafka 3.8 (KRaft, one node) and RabbitMQ 3.13, with a PVC each. Kafka and RabbitMQ keep their data across a pod restart. |
| Security | Pods run as non-root with a read-only root filesystem for the API, no privilege escalation, all capabilities dropped and the `RuntimeDefault` seccomp profile; the namespace enforces the `baseline` Pod Security level and warns on `restricted`. A NetworkPolicy lets only the API reach Postgres, Kafka and RabbitMQ (kind's `kindnet` enforces it). |
| Config and secrets | A ConfigMap (`sole-config`) and a Secret (`sole-secrets`). The kind overlay generates the demo Secret from `deploy/k8s/overlays/kind/secrets.env` (git-ignored, written by `scripts/kind-secrets.sh`, random signing key). A real cluster brings its own Secret under the same name. |
| Reaching it | `deploy/kind/cluster.yaml` publishes three NodePorts on `127.0.0.1` only: the web app on **8108**, the API on **5228**, RabbitMQ's management UI on **15718**. nginx reaches the API as `api.sole.svc.cluster.local` through the cluster DNS. |

The base manifests (`deploy/k8s/base`) carry the production defaults (email confirmation required, no dev mailbox, 20 auth
requests a minute). The kind overlay relaxes exactly what a demo needs: no email confirmation, the dev mailbox on (kept in
Postgres, so both replicas see the same messages), a delay on the final mark so the smoke script can land a refund inside the
window, a fast drawer.

## Authentication and authorization

| Piece | How it works |
| --- | --- |
| Accounts | Passwords need 10+ characters with upper case, lower case and a digit, and 4 distinct characters, at most 128 (anything longer is refused before it is hashed). They are hashed with **argon2id** (19 MiB, 2 passes, per-hash salt). Five wrong passwords (or codes) lock the account for 15 minutes (`429`). A wrong password and an unknown email give the same `401`, and take the same time. |
| Email | Registration mails a confirmation link; with `AUTH_REQUIRE_CONFIRMED_EMAIL` on (the default) sign-in is refused (`403`) until it is opened. "Forgot password" mails a one-time reset link and answers `202` for every address, known or not. The links carry an HMAC token bound to the user, the purpose, an expiry and the user's *security stamp*, which rotates when the password changes, so each link works once. Messages go through a mailer interface: SMTP when `MAIL_HOST` is set, the log otherwise. |
| Two-factor | TOTP (RFC 6238, any authenticator app): `POST /api/auth/mfa/setup` returns the key and an `otpauth://` URI, `enable` proves it with a code and returns eight one-time recovery codes, `disable` needs the password and a code. Once on, sign-in needs a code (`401 A verification code is required.`). |
| Access token | JWT, 15 minutes, held **in memory** by the Angular app and sent as `Authorization: Bearer`. HS256 with a key of at least 32 bytes, or **RS256** when `JWT_RSA_PRIVATE_KEY_PEM` is set, in which case the public key is served at `/.well-known/jwks.json`. Only the configured algorithm is accepted (no `none`, no HS/RS confusion). |
| Revocation | Each token carries the user's session version. A password change, a reset, turning two-factor off and **"sign out everywhere"** bump it (one atomic `UPDATE`), and every token sold earlier stops working on every replica within `JWT_REVOCATION_CACHE_SECONDS` (default 5). |
| Refresh token | 384 random bits in an `HttpOnly`, `SameSite=Strict`, `Path=/api/auth` cookie (`Secure` over HTTPS). Only its SHA-256 is stored. Every refresh **rotates** it with a conditional `UPDATE`, so two concurrent refreshes with the same token cannot both win. Presenting a token that was already used revokes the whole family. Refresh and logout also need an `X-Requested-With: sole-web` header, so another site cannot trigger them. |
| Roles | `Customer` (default at registration), `Staff`, `Admin`. An administrator is seeded from `ADMIN_EMAIL` / `ADMIN_PASSWORD` on first start. |
| Rules | Anonymous: `/api/health*`, `/api/auth/register|login|refresh|logout` and the email flows. Every other route needs a token. Any signed-in user: read raffles, enter, read and refund **their own** orders and claims. `Staff` and `Admin`: open raffles, read everything, read the event log and the dead letters. Another user's order or claim is `404`, not `403`, so ids do not leak. The owner of an order is the caller; a body field cannot set it. |
| Hardening | A rate limit on `/api/auth/*` per client (`RATE_LIMIT_AUTH_PER_MINUTE`), **counted in Postgres** so every replica shares one count, security headers on every response (`nosniff`, `DENY`, strict CSP on the API, `no-referrer`), a correlation id echoed in `X-Correlation-Id` and in every problem body, JSON logs (`LOG_JSON`), forwarded headers trusted only when `FORWARDED_HEADERS` is on, a panic handler, and a readiness endpoint that checks every module schema. |

## Run it

You need Go 1.26, Docker, [kind](https://kind.sigs.k8s.io), kubectl and Node 22 (for the Angular app and the browser specs).

```bash
make test-unit                  # no Docker
make deps                       # Postgres, Kafka and RabbitMQ in Docker, for `go run` and the integration tests
make test-integration           # a real Postgres: the database sole_test
make cluster-up                 # builds both images, creates the kind cluster, applies the manifests, waits until Ready
./scripts/smoke.sh              # last line: golang-sole smoke passed
npm ci && npx playwright install chromium
npx playwright test             # the browser specs against http://127.0.0.1:8108
./scripts/resilience.sh         # kills an API replica and RabbitMQ in the middle of a raffle (about two minutes)
make cluster-down
```

| Service | On your machine |
| --- | --- |
| Web (nginx, proxies `/api`) | <http://127.0.0.1:8108> |
| API (`/api/health`, `/api/health/ready`) | <http://127.0.0.1:5228> |
| RabbitMQ management (`platform` / `platform`) | <http://127.0.0.1:15718> |
| `make deps`: Postgres / Kafka / RabbitMQ | 5488 / 9138 / 5718 (management 15718, so stop the cluster first) |

Sign in as `admin@sole.local` / `SoleAdmin2026` in the demo cluster, or register a customer.

## Try it with curl

```bash
API=http://127.0.0.1:5228
login() { curl -s -X POST $API/api/auth/login -H 'content-type: application/json' -d "{\"email\":\"$1\",\"password\":\"$2\"}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["accessToken"])'; }
ADMIN="Authorization: Bearer $(login admin@sole.local SoleAdmin2026)"

curl -s -X POST $API/api/auth/register -H 'content-type: application/json' \
  -d '{"email":"ana@example.com","password":"Passw0rd-Demo1"}'      # 201; its accessToken goes in $ANA below
ANA="Authorization: Bearer <accessToken>"

DRAW_AT=$(python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=2)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
curl -s -H "$ADMIN" -X POST $API/api/raffles \
  -H 'content-type: application/json' \
  -d "{\"code\":\"VOLT-01\",\"name\":\"Volt One\",\"colorway\":\"Volt\",\"brand\":\"Nimbus\",\"pairsTotal\":2,\"priceCents\":8000,\"drawAt\":\"$DRAW_AT\"}"
# the response shows seedHash (the commitment) and seed: null until the draw

curl -s -H "$ANA" -X POST $API/api/orders \
  -H 'content-type: application/json' \
  -d '{"raffleCode":"VOLT-01","reference":"ENT-1001","pairs":1}'
# 202, status Requested, then Entered: nothing is taken yet. One entry per person per raffle. Use the returned id.

curl -s -H "$ANA" $API/api/orders/<id>               # Entered now; after the draw: Sold (with a claim number) or Rejected ("Not drawn: place 3 of 7")
curl -s -H "$ANA" $API/api/raffles/VOLT-01/draw      # after the draw: the seed, whether it matches the hash, and your entry's key and rank
curl -s -H "$ANA" $API/api/claims/<id>
curl -s -H "$ADMIN" $API/api/orders/<id>/events        # the Kafka log for this order

curl -s -H "$ANA" -X POST $API/api/orders/<id>/refund  # before the draw: withdraws the entry; after it: the pairs go back, the claim stays
```

To check a draw yourself: `sha256(seed)` must equal the `seedHash` you saw when the raffle opened; each entry's key is
`sha256("<seed>:<orderId>")`; sort the keys ascending and give pairs out in that order, each entry taking its pairs if they
still fit. That reproduces every outcome.

An order is validated at the door: entrant reference required (at most 40 characters), 1 to 6 pairs. The draw time must be in
the future when the raffle opens, and entries after it are refused (409, or `Closed` if one slips through a race). A second
entry by the same person is accepted at the door and rejected by the raffle with `Already entered`. The poison match is a
case-sensitive substring: `ENT-POISON-7` and any longer token containing `POISON` match.

## Develop

```bash
make deps && make run                 # the API on :5228 against the local dependencies (migrates itself)
npx nx serve web                      # http://localhost:4308, proxies /api to :5228
WEB_URL=http://localhost:4308 npx playwright test
make lint                             # gofmt and go vet
make logs                             # follow the API logs of every replica in the cluster
make cluster-status
```

## What is tested, and where it runs

- **Unit tests** (no Docker, under a second each): the draw (the commitment is the SHA-256 of the seed, the order does not
  depend on how entries arrive, another seed gives another order, winners never take more than the pairs, first fit lets a
  smaller entry win after a bigger one fails, and across 4,000 seeds each of 20 entrants wins about 25% of the time for 5
  pairs), password hashing and policy, RFC 6238 test vectors for two-factor, access tokens (round trip, tampering, expiry
  inside and beyond the skew, `alg=none`, a token signed with another key, issuer and audience, RS256 and the algorithm
  confusion attack, the JWKS), one-time email tokens (single use, bound to the user, the purpose, the state and an expiry),
  the retry policy and the poison rule, the wire names of the messages and of the Kafka envelope, the order rules and
  overflow, the problem+json responses, the correlation id and security headers, owner-or-staff access, config validation.
- **Integration tests** (`internal/integration`, a real Postgres, the real middleware chain and routes, brokers replaced by a
  loop that hands each outbox row to the handlers its routing key is bound to): register, login, lockout, refresh rotation,
  replay revoking the family, two and six concurrent refreshes (exactly one wins), logout, change password, reset, email
  confirmation, the full two-factor lifecycle, the shared rate limit, RS256 and the JWKS, every route's 401/403/404 and
  ownership, paging, validation at the door; the raffle (entering takes no stock, one entry per person, a closed raffle, the draw
  against an independent recomputation, five replicas drawing at once draw once, leaving before and after the draw, **eight
  rounds of withdrawals racing the draw never lose or double-count a pair**, the draw view hides the seed until the draw); the
  whole saga (entry to sold and a claim, losers told their place, poison dead-lettered and the pairs released, refund before
  and after the draw, a late final mark that loses to a refund, a repeated delivery that has no second effect, a final mark that
  arrives before the held mark and is retried); the outbox (several relays drain disjoint batches, a failed publish rolls the
  batch back, trimming keeps the inbox longer than the outbox) and migrations (six replicas migrating at once apply each file
  once, a broken file rolls back). The racing-withdrawal test caught a real bug while it was written: loading the entries
  without locking let a withdrawal delete a row between the draw reading it and writing its outcome; the draw now locks them.
  Both suites run with the race detector in CI.
- **`scripts/smoke.sh`** drives the cluster over HTTP: authentication and ownership first (401, 403, 404), a password reset that
  ends the old sessions, validation (including a draw time in the past), the happy path (enter, the draw runs by itself, sold,
  the seed matches its commitment, Kafka log), a one-pair draw with three entrants (one sold, two "Not drawn", recomputed from
  the seed), one entry per person, withdrawing and a late entry (409), the poison reference (it wins the draw, rejected,
  `claims.work.dlq` gains one message, pairs restored), the dead letters listed and replayed, and a refund that lands between
  the claim and the final mark.
- **Playwright** (`e2e/specs`, 15 specs, Chromium, against the web port of the cluster): sign-in, the password policy, a customer
  who enters and sees only their own orders, the session surviving a reload through the cookie, a password reset through the
  emailed link, two-factor sign-in, "sign out everywhere", refusal messages and one entry per person, a poison entry that wins
  and is rejected, **a draw checked end to end** (the revealed seed hashes to the commitment and the winner is the lowest key),
  withdrawing before the draw, the Kafka event log.
- **`scripts/resilience.sh`** is the one only Kubernetes makes possible: it kills an API replica and the RabbitMQ pod in the
  middle of a raffle, enters a third person while the broker is down, and checks afterwards that the draw happened once, one
  pair was sold, two entrants were told they were not drawn, exactly one claim exists, and the seed still matches its commitment.
- **CI** (`.github/workflows/ci.yml`) has two jobs. `verify`: gofmt and `go vet`, unit and integration tests with `-race`
  (Postgres 17 service container), `govulncheck`, `npm audit`, Prettier, the Angular type check, lint and build, and the
  Kustomize overlay rendering. `e2e`: creates a kind cluster, runs `make cluster-up`, the smoke script, the browser specs and
  the resilience script, and uploads cluster state, logs and traces on failure. Dependabot watches Go modules, npm, Actions and
  Docker.

## API

JSON over HTTP, the same as dotnet-sole's. Everything except health and `/api/auth/register|login|refresh|logout` and the
email flows needs a bearer token.

| Method and path | What it does | Responses |
| --- | --- | --- |
| `GET /api/health`, `GET /api/health/ready` | Liveness; readiness (every module schema reachable). | `200`, `503` |
| `POST /api/auth/register` | Creates a customer and mails a confirmation link. Signs them in unless confirmation is required. | `201` signed in, `202` check your inbox, `400` policy, `409` email taken, `429` |
| `POST /api/auth/login` | Signs in (with `code` when two-factor is on); sets the refresh cookie. | `200`, `401`, `403` unconfirmed, `429` locked or rate limited |
| `POST /api/auth/refresh`, `POST /api/auth/logout` | Rotate or revoke the refresh cookie. Need `X-Requested-With: sole-web`. | `200`/`204`, `400` header missing, `401` |
| `POST /api/auth/confirm-email`, `resend-confirmation` | Confirm an address with the emailed token; ask for a new link (always `202`). | `204`/`202`, `400` |
| `POST /api/auth/forgot-password`, `reset-password` | Mail a reset link (always `202`); set a new password with it (ends every session). | `202`/`204`, `400` |
| `POST /api/auth/change-password`, `logout-everywhere` | Signed in: change the password (ends the other sessions and returns a fresh one); end every session on every device. | `200`/`204`, `400`, `401` |
| `GET /api/auth/mfa`, `POST /api/auth/mfa/setup`, `enable`, `disable` | Two-factor status and lifecycle. | `200`/`204`, `400`, `409` |
| `GET /.well-known/jwks.json` | The public key set when RS256 is on. | `200`, `404` |
| `GET /api/auth/me` | The caller. | `200`, `401` |
| `POST /api/raffles` | Opens a raffle with its pairs, a price in cents and a draw time in the future. Staff. The server picks the seed and shows only `seedHash` until the draw. | `201`, `400` invalid, `403`, `409` code exists |
| `GET /api/raffles`, `GET /api/raffles/{code}` | Raffles with the live pairs left, the entry count, the status (`Open`/`Drawn`), `seedHash`, and `seed` once drawn. | `200`, `404` |
| `GET /api/raffles/{code}/draw` | What it takes to check a draw: the seed, whether it matches the hash, the totals, and the caller's own entries with their key, rank and outcome (staff: every entry). | `200`, `401`, `404` |
| `POST /api/orders` | Enters the raffle for the caller (1 to 6 pairs) and starts the saga. The entry takes no pairs; the draw decides. | `202` Requested, `400` invalid, `404` unknown raffle, `409` duplicate reference or the draw time has passed |
| `GET /api/orders?raffleCode=&page=&pageSize=` | The caller's orders, newest first, a page at a time (`pageSize` at most 100, default 100). Staff: everyone's. The total is in `X-Total-Count`. | `200` |
| `GET /api/orders/{id}` | One order and its status. Owner or staff. | `200`, `404` |
| `POST /api/orders/{id}/refund` | Moves an entered, held or sold order to `Refunded`: withdraws the entry before the draw, or puts the pairs back after it. The claim is not touched. Owner or staff. | `202`, `404`, `409` wrong status |
| `GET /api/orders/{id}/events` | The Kafka event log for one order. Staff. | `200`, `403` |
| `GET /api/admin/dead-letters`, `POST /api/admin/dead-letters/{queue}/replay` | What waits in each dead-letter queue, with the first messages; publish them again. Staff. A message that still fails for the same reason is dead-lettered again. | `200`, `403`, `503` brokers off |
| `GET /api/claims/{orderId}` | The claim row. Owner or staff. | `200`, `404` |

Errors come back as `application/problem+json` with a `title` and the `correlationId`.

## Configuration

The process is configured from the environment (the ConfigMap and Secret in the cluster). Defaults work against `make deps`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://sole:sole@localhost:5488/sole?sslmode=disable` | The one Postgres database; each module has its own schema. |
| `RABBITMQ_URL` / `KAFKA_BROKERS` | `amqp://platform:platform@localhost:5718/` / `localhost:9138` | Work queue and event log. |
| `MESSAGING_ENABLED` | `true` | Switch the relay and the consumers off (the integration tests do). |
| `JWT_KEY` | none | HS256 signing key, at least 32 bytes. Required; the process refuses to start without it (or `JWT_RSA_PRIVATE_KEY_PEM`). |
| `JWT_ACCESS_MINUTES` / `JWT_REFRESH_DAYS` / `JWT_SECURE_COOKIES` | `15` / `14` / `false` | Token lifetimes; set `JWT_SECURE_COOKIES` when TLS ends in front of the API. |
| `JWT_RSA_PRIVATE_KEY_PEM` / `JWT_KEY_ID` | unset / `sole-1` | Sign with RS256 and publish the public key at `/.well-known/jwks.json`. |
| `JWT_REVOCATION_CACHE_SECONDS` | `5` (`2` in the demo) | How long a replica trusts a cached session version before re-reading it. |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | unset | The administrator created on first migration. |
| `AUTH_REQUIRE_CONFIRMED_EMAIL` / `AUTH_PUBLIC_URL` | `true` / `http://localhost:4308` | Sign-in waits for email confirmation; the address links in emails point to. |
| `MAIL_HOST` / `MAIL_PORT` / `MAIL_USER` / `MAIL_PASSWORD` / `MAIL_FROM` / `MAIL_START_TLS` | unset | SMTP for confirmation and reset emails. Unset means the log only. |
| `MAIL_DEV_MAILBOX` | `false` (`true` in the demo) | Keep the last messages in Postgres and serve them at `/api/dev/mailbox`. Never on in production. |
| `RATE_LIMIT_AUTH_PER_MINUTE` | `20` (`1000` in the demo) | Requests a minute per client to `/api/auth/*`. |
| `FORWARDED_HEADERS` | `false` (`true` behind nginx) | Trust `X-Forwarded-For` and `-Proto` from the reverse proxy. |
| `OUTBOX_RETENTION_DAYS` / `INBOX_RETENTION_DAYS` | `7` / `30` | Sent outbox rows and old inbox claims are deleted after this long. The inbox is kept longer because it must outlive any redelivery. |
| `ORDERS_FINAL_MARK_DELAY_MS` | `0` (`1500` in the demo) | Milliseconds the final mark waits. A latency knob so the smoke script can land a refund inside the window. |
| `RAFFLES_DRAW_INTERVAL_MS` | `1000` (`500` in the demo) | How often each replica looks for raffles whose draw time has come. |
| `MIGRATE_ON_START` | `true` | `false` in the cluster, where the init container migrates. |
| `LISTEN_ADDR` / `LOG_JSON` | `:8080` / `false` | |
| `TEST_DATABASE_URL` (tests only) | `postgres://sole:sole@localhost:5488/sole_test?sslmode=disable` | The Postgres the integration tests use; the database is created if it is missing. |

## Look inside and fix things

```bash
make cluster-status                                   # pods, services, disruption budgets
kubectl -n sole logs -f deploy/api --all-containers   # JSON logs (one line per request, one per worker event)
kubectl -n sole exec postgres-0 -- psql -U sole -d sole -c 'select routing_key, sent_at from orders.outbox_messages order by id desc limit 10'
kubectl -n sole delete pod -l app.kubernetes.io/name=api --wait=false   # a replica dies; the Service keeps answering
open http://127.0.0.1:15718                           # RabbitMQ management, platform / platform
```

- **Dead-lettered messages** sit in `claims.work.dlq` (and the two other `.dlq` queues). Staff can list and replay them from
  `/api/admin/dead-letters`, or inspect them in the management UI. Retries wait in `<queue>.retry` for 2 seconds.
- **A port is already in use**: `lsof -nP -i :8108`. The kind cluster and `make deps` both want RabbitMQ's management port
  (15718); stop one before starting the other.
- **Start clean**: `make cluster-down && make cluster-up` (the PVCs go with the cluster) and `make deps-down` for the
  dependencies.
- **A pod is not Ready after `make cluster-up`**: `kubectl -n sole describe pod <name>` and the logs of the `migrate` init
  container, which is where a wrong database password shows up first.

## Known limits

- It runs on **kind**, a one-node cluster on your machine. Nothing here is a cloud deployment: there is no ingress controller,
  no TLS, no autoscaler (the HPA needs a metrics server) and one broker of each kind. The manifests are plain Kubernetes and
  Kustomize, and the stateful services are the part you would replace with managed ones.
- The per-person rule is per account. Nothing stops one person registering a second account, so the draw is only as fair as
  the one-entry-per-person rule is hard to dodge. The seed is generated by the server and committed as a hash before any
  entry, so the operator cannot choose an order after seeing the entries, but it sits in the database until the draw, so someone
  with database access could read the order early; a production system would keep it in a vault or use an external randomness
  beacon.
- The draw runs in one transaction per raffle. That is fine for thousands of entries and not for millions. Pairs that come back
  after the draw (a refund, a poison rejection) go back on the shelf; they are not re-raffled to the entries that were not drawn.
- Revocation is eventual by design: a replica may honour a revoked access token for up to `JWT_REVOCATION_CACHE_SECONDS` seconds.
  Set it to `0` to check the database on every request.
- Delivery is at-least-once, not exactly-once. The inbox makes repeats harmless for the saga; the Kafka log can hold a
  duplicate event after a relay crash.
- `GET .../events` reads the Kafka topic for one key, so it scans the topic. Fine here, not something to expose at scale.
- Sign-in is email and password plus optional TOTP. There are no external identity providers and no WebAuthn passkeys.
- The browser specs drive Chromium at a desktop size only, and the `e2e` CI job needs a few minutes to build and start the cluster.

## Layout

```
cmd/api            the binary: `serve` and `migrate`
internal/app       wiring, the middleware chain, the background workers
internal/identity  users, roles, JWT, refresh tokens, two-factor, email flows      (schema identity)
internal/orders    order rules, the saga's first step and the event handler          (schema orders)
internal/raffles   the raffle, the committed seed, the draw, the holds               (schema raffles)
internal/claims    claims                                                           (schema claims)
internal/platform  config, pgdb (pool, migrations), httpx, messaging (outbox, relay, consumers, Kafka log, dead letters)
internal/integration  the integration suite     internal/testutil  the shared test database
apps/web           Angular 21, styled with Tailwind 4 utilities only (shared with dotnet-sole)
e2e                Playwright specs
deploy/k8s         Kustomize base and the kind overlay       deploy/kind  the cluster     deploy/compose.deps.yaml  local dependencies
scripts            smoke.sh, resilience.sh, kind-secrets.sh
Dockerfile         two targets: api (distroless) and web (nginx)
```

## Other systems in this series

- [dotnet-sole](https://github.com/joshbarros/dotnet-sole): this system on ASP.NET Core and Docker Compose, with the same API and UI.
- [golang-mint](https://github.com/joshbarros/golang-mint): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-pledge](https://github.com/joshbarros/golang-pledge): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-bistro](https://github.com/joshbarros/golang-bistro): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-flash](https://github.com/joshbarros/golang-flash): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-encore](https://github.com/joshbarros/golang-encore): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-atelier](https://github.com/joshbarros/golang-atelier): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-freight](https://github.com/joshbarros/golang-freight): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-clinic](https://github.com/joshbarros/golang-clinic): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-warehouse](https://github.com/joshbarros/golang-warehouse): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-academy](https://github.com/joshbarros/golang-academy): the same kind of system for a different scarce number, also Go on Kubernetes.
- The `dotnet-*` series (freight, clinic, ledger, warehouse, academy, compute, hotel, tickets, carbon, charging, promo, mint, pledge, bistro, flash, encore, atelier, sole) has the whole family on ASP.NET Core; each adds one more way to keep a scarce number honest.
