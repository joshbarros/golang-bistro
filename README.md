# golang-bistro

Table reservations with a waitlist, written in **Go** and run on **Kubernetes**. It is the Go and Kubernetes version of
[dotnet-bistro](https://github.com/joshbarros/dotnet-bistro): the same saga, the same HTTP API and the same Angular UI, so the
browser specs and the smoke script of the .NET system run against this one unchanged.

The scarce number is the **covers left** in a dinner sitting, and the **sittings** module is its only writer. A reservation
asks for 1 to 6 covers. When the sitting is full a guest can **join the waitlist** instead of being turned away: the line is
first come, first served, and when covers come back (a cancellation, a refund, a poison rejection) the same transaction that
gives them back seats the people waiting, **strictly in order**: if the party at the head of the line does not fit yet, nobody
behind it jumps ahead, and new reservations cannot slip past a line either. Leaving the line is a conditional `DELETE` that
races a promotion safely: exactly one of them wins the row, and either way every cover is free or held, never both, never
lost. Two further rules hold in the same transactions: a guest may hold at most **`max_per_guest`** covers of a sitting (an
atomic upsert that only moves while the new total fits, rolled back with a failed attempt), and the covers are taken with a
single conditional `UPDATE`. A guest reference that contains the poison token `POISON` is committed as rejected by the
confirmations module, the message is dead-lettered, and a compensating command gives the covers back and seats the line. A
later refund does the same, but it does not roll back the confirmation that was already issued.

What is different here is how it runs. **Kubernetes is what runs the stack**: Postgres, Kafka, RabbitMQ, **two API replicas**
and two web replicas, started in a local [kind](https://kind.sigs.k8s.io) cluster with Kustomize manifests. Nothing is faked
for the demo: the same pipeline (`make cluster-up`, the smoke script, the browser specs, a script that kills pods mid-booking)
runs in CI on a fresh kind cluster.

![The Bistro UI signed in as the administrator: a full sitting with a party seated and two parties waitlisted in order](docs/screenshot.png)

*The UI reads everything from the API; shown signed in as the seeded administrator on the cluster, on a sitting that is full:
the second party asked to wait and sits at number 1 in the line, the third is number 2.*

## Order statuses

```mermaid
flowchart LR
    Requested -->|covers held| Held
    Requested -->|full and the guest asked to wait| Waitlisted
    Waitlisted -->|covers come back, in order| Held
    Waitlisted -->|leaves the line| Refunded
    Requested -->|full without the waitlist, or over the limit: no hold written| Rejected
    Waitlisted -->|over the limit at promotion| Rejected
    Held -->|confirmation issued| Sold
    Held -->|poison reference: confirmation rejected| Rejected
    Held -->|refund| Refunded
    Sold -->|refund| Refunded
```

`Refunded` and `Rejected` (after a hold) put the covers back through a `ReleaseCovers` command, and Sittings releases a hold at most once, gives the guest their allowance back with it, and then seats the waitlist. For a waitlisted order the same command just removes its place in the line. The confirmation has its own status (`Issued` or `Rejected`) and a refund never changes it. If the refund gets there before the final mark, the late mark finds the order is no longer `Held` and does nothing.

## How the modules talk

```mermaid
sequenceDiagram
    participant C as Client
    participant O as Orders
    participant T as Sittings
    participant E as Confirmations

    C->>O: POST /api/orders
    Note over O: order + outbox row, one transaction
    O-->>C: 202 Requested
    O->>T: covers.reserve (relay → RabbitMQ, copy to Kafka)
    Note over T: inbox claim, per-guest limit upsert, conditional UPDATE (not while a line exists), hold, outbox row
    alt covers left cover the order and the guest is within the limit
        T->>E: covers.held
        T->>O: covers.held (Requested → Held)
        alt reference is clean
            Note over E: confirmation, outbox row
            E->>O: confirmation.issued (Held → Sold)
        else reference contains POISON
            Note over E: committed Rejected, then the message is dead-lettered
            E->>T: covers.release (compensating command)
            E->>O: confirmation.rejected (Held → Rejected)
            T->>O: covers.released
        end
    else sitting full and the order asked to wait
        Note over T: the attempt is rolled back, then a waitlist row is written
        T->>O: covers.waitlisted (Requested → Waitlisted, number N in line)
        Note over T: later, a release promotes the head of the line: hold + covers.held
    else sitting full without the waitlist, or the guest is over the limit
        Note over T: the attempt is rolled back, so a limit increment never outlives a failed reserve
        T->>O: covers.short (Requested → Rejected, no hold, reason Short or Cap)
    end
    C->>O: POST /api/orders/{id}/refund
    O->>T: covers.release (Held or Sold → Refunded)
    T->>O: covers.released
```

| Module | Owns | Consumes | Emits |
| --- | --- | --- | --- |
| **identity** (`internal/identity`) | `identity` schema: users, roles, refresh tokens. | | |
| **orders** (`internal/orders`) | `orders.orders`: status, covers, total, guest reference. Validation, the poison rule, the total. | `covers.held`, `covers.short`, `covers.waitlisted`, `confirmation.issued`, `confirmation.rejected`, `covers.released` | `covers.reserve`, `covers.release` (refund) |
| **sittings** (`internal/sittings`) | `sittings.sittings.covers_left` (the only writer after the sitting opens), `sittings.guests` (what each guest holds), `sittings.cover_holds` and the `sittings.waitlist`. | `covers.reserve`, `covers.release` (prefetch 1) | `covers.held`, `covers.short`, `covers.waitlisted`, `covers.released` |
| **confirmations** (`internal/confirmations`) | `confirmations.confirmations`. | `covers.held` | `confirmation.issued`, `confirmation.rejected`, `covers.release` (poison) |
| **platform** (`internal/platform`) | The relay, the consumer loop with retry and dead-letter, the Kafka log, problem+json and the middleware, config, migrations. Each module owns its own `outbox_messages` and `inbox_messages` tables. | | |

RabbitMQ topology is declared in code (`internal/platform/messaging/rabbit.go`): exchange `bistro.work`, one queue per module
(`sittings.commands`, `confirmations.work`, `orders.events`), a `.retry` queue per module (2 s TTL, then back to the main queue, 3
attempts), and a `.dlq` per module behind `bistro.dlx`. A transient failure retries; a poison message goes straight to the
dead-letter queue after its rejection is committed. After a trip through the retry queue the broker's routing key is the queue
name, so the relay stores the original key in the message `type` and the consumer dispatches on that. Kafka has one topic,
`bistro.events`, one partition, replication factor 1, keyed by order id.

## Architecture

One binary (`cmd/api`), four modules in `internal/`. Each module has its own Postgres **schema** (`identity`, `orders`,
`sittings`, `confirmations`), its own embedded SQL migrations and its own outbox and inbox tables. No module imports another; the one
cross-module read (a sitting's existence and price) goes through a small interface that the orders module declares and the sittings module
satisfies, and `internal/app` is the only package that knows all of them.

```mermaid
flowchart TB
    main["cmd/api: serve | migrate"] --> app["internal/app: wiring, middleware chain, workers"]
    app --> identity & orders & sittings & confirmations
    identity["internal/identity: users, roles, JWT, refresh tokens, two-factor"]
    orders["internal/orders: schema orders"]
    sittings["internal/sittings: schema sittings, the covers, the cap and the waitlist"]
    confirmations["internal/confirmations: schema confirmations"]
    identity & orders & sittings & confirmations --> platform["internal/platform: httpx, messaging, pgdb, config"]
    orders -. "Catalog.Find" .-> sittings
```

Modules still talk only through messages: RabbitMQ carries the commands, Kafka keeps the event log, and the outbox and inbox
make delivery at-least-once and consumption idempotent. The dependencies are small on purpose: `pgx`, `amqp091-go`,
`franz-go`, `golang-jwt` and `x/crypto`; the HTTP layer is the standard library's `net/http` mux.

## On Kubernetes

```mermaid
flowchart LR
    host["127.0.0.1:8111 · :5231 · :15721"] -->|NodePort| web & api & mgmt
    subgraph ns["namespace bistro (kind: one node)"]
        web["web ×2: nginx + Angular"] -->|/api| api
        api["api ×2: Go (init container: migrate)"] --> pg[("postgres StatefulSet")]
        api --> kafka[("kafka StatefulSet (KRaft)")]
        api --> rmq[("rabbitmq StatefulSet")]
        mgmt["RabbitMQ management"] --- rmq
    end
```

| What | How it is set up |
| --- | --- |
| API | A Deployment of **2 replicas**, `maxUnavailable: 0`, a PodDisruptionBudget (`minAvailable: 1`), spread over nodes. Nothing in it assumes a single instance: the relay claims outbox rows with `SKIP LOCKED`, the stock is taken with a conditional `UPDATE`, the auth rate limit and the session version live in Postgres. |
| Migrations | An **init container** runs `api migrate` before any replica serves (each module migrates behind a Postgres advisory lock, so several pods starting together are safe) and the API runs with `MIGRATE_ON_START=false`. |
| Probes | `startupProbe` and `livenessProbe` on `/api/health`; `readinessProbe` on `/api/health/ready`, which checks that every module's schema is there. |
| Data stores | One StatefulSet each for Postgres 17, Kafka 3.8 (KRaft, one node) and RabbitMQ 3.13, with a PVC each. Kafka and RabbitMQ keep their data across a pod restart. |
| Security | Pods run as non-root with a read-only root filesystem for the API, no privilege escalation, all capabilities dropped and the `RuntimeDefault` seccomp profile; the namespace enforces the `baseline` Pod Security level and warns on `restricted`. A NetworkPolicy lets only the API reach Postgres, Kafka and RabbitMQ (kind's `kindnet` enforces it). |
| Config and secrets | A ConfigMap (`bistro-config`) and a Secret (`bistro-secrets`). The kind overlay generates the demo Secret from `deploy/k8s/overlays/kind/secrets.env` (git-ignored, written by `scripts/kind-secrets.sh`, random signing key). A real cluster brings its own Secret under the same name. |
| Reaching it | `deploy/kind/cluster.yaml` publishes three NodePorts on `127.0.0.1` only: the web app on **8111**, the API on **5231**, RabbitMQ's management UI on **15721**. nginx reaches the API as `api.bistro.svc.cluster.local` through the cluster DNS. |

The base manifests (`deploy/k8s/base`) carry the production defaults (email confirmation required, no dev mailbox, 20 auth
requests a minute). The kind overlay relaxes exactly what a demo needs: no email confirmation, the dev mailbox on (kept in
Postgres, so both replicas see the same messages), a delay on the final mark so the smoke script can land a refund inside the
window.

## Authentication and authorization

| Piece | How it works |
| --- | --- |
| Accounts | Passwords need 10+ characters with upper case, lower case and a digit, and 4 distinct characters, at most 128 (anything longer is refused before it is hashed). They are hashed with **argon2id** (19 MiB, 2 passes, per-hash salt). Five wrong passwords (or codes) lock the account for 15 minutes (`429`). A wrong password and an unknown email give the same `401`, and take the same time. |
| Email | Registration mails a confirmation link; with `AUTH_REQUIRE_CONFIRMED_EMAIL` on (the default) sign-in is refused (`403`) until it is opened. "Forgot password" mails a one-time reset link and answers `202` for every address, known or not. The links carry an HMAC token bound to the user, the purpose, an expiry and the user's *security stamp*, which rotates when the password changes, so each link works once. Messages go through a mailer interface: SMTP when `MAIL_HOST` is set, the log otherwise. |
| Two-factor | TOTP (RFC 6238, any authenticator app): `POST /api/auth/mfa/setup` returns the key and an `otpauth://` URI, `enable` proves it with a code and returns eight one-time recovery codes, `disable` needs the password and a code. Once on, sign-in needs a code (`401 A verification code is required.`). |
| Access token | JWT, 15 minutes, held **in memory** by the Angular app and sent as `Authorization: Bearer`. HS256 with a key of at least 32 bytes, or **RS256** when `JWT_RSA_PRIVATE_KEY_PEM` is set, in which case the public key is served at `/.well-known/jwks.json`. Only the configured algorithm is accepted (no `none`, no HS/RS confusion). |
| Revocation | Each token carries the user's session version. A password change, a reset, turning two-factor off and **"sign out everywhere"** bump it (one atomic `UPDATE`), and every token sold earlier stops working on every replica within `JWT_REVOCATION_CACHE_SECONDS` (default 5). |
| Refresh token | 384 random bits in an `HttpOnly`, `SameSite=Strict`, `Path=/api/auth` cookie (`Secure` over HTTPS). Only its SHA-256 is stored. Every refresh **rotates** it with a conditional `UPDATE`, so two concurrent refreshes with the same token cannot both win. Presenting a token that was already used revokes the whole family. Refresh and logout also need an `X-Requested-With: bistro-web` header, so another site cannot trigger them. |
| Roles | `Customer` (default at registration), `Staff`, `Admin`. An administrator is seeded from `ADMIN_EMAIL` / `ADMIN_PASSWORD` on first start. |
| Rules | Anonymous: `/api/health*`, `/api/auth/register|login|refresh|logout` and the email flows. Every other route needs a token. Any signed-in user: read sittings, book, read and refund **their own** orders and confirmations. `Staff` and `Admin`: open sittings, read everything, read the event log and the dead letters. Another user's order or confirmation is `404`, not `403`, so ids do not leak. The owner of an order is the caller; a body field cannot set it. |
| Hardening | A rate limit on `/api/auth/*` per client (`RATE_LIMIT_AUTH_PER_MINUTE`), **counted in Postgres** so every replica shares one count, security headers on every response (`nosniff`, `DENY`, strict CSP on the API, `no-referrer`), a correlation id echoed in `X-Correlation-Id` and in every problem body, JSON logs (`LOG_JSON`), forwarded headers trusted only when `FORWARDED_HEADERS` is on, a panic handler, and a readiness endpoint that checks every module schema. |

## Run it

You need Go 1.26, Docker, [kind](https://kind.sigs.k8s.io), kubectl and Node 22 (for the Angular app and the browser specs).

```bash
make test-unit                  # no Docker
make deps                       # Postgres, Kafka and RabbitMQ in Docker, for `go run` and the integration tests
make test-integration           # a real Postgres: the database bistro_test
make cluster-up                 # builds both images, creates the kind cluster, applies the manifests, waits until Ready
./scripts/smoke.sh              # last line: golang-bistro smoke passed
npm ci && npx playwright install chromium
npx playwright test             # the browser specs against http://127.0.0.1:8111
./scripts/resilience.sh         # kills an API replica and RabbitMQ in the middle of a sitting (about two minutes)
make cluster-down
```

| Service | On your machine |
| --- | --- |
| Web (nginx, proxies `/api`) | <http://127.0.0.1:8111> |
| API (`/api/health`, `/api/health/ready`) | <http://127.0.0.1:5231> |
| RabbitMQ management (`platform` / `platform`) | <http://127.0.0.1:15721> |
| `make deps`: Postgres / Kafka / RabbitMQ | 5491 / 9141 / 5721 (management 15721, so stop the cluster first) |

Sign in as `admin@bistro.local` / `BistroAdmin2026` in the demo cluster, or register a customer.

## Try it with curl

The web app is at <http://127.0.0.1:8111> (`admin@bistro.local` / `BistroAdmin2026` in the demo stack, or register a customer). From the shell:

```bash
API=http://127.0.0.1:5231
login() { curl -s -X POST $API/api/auth/login -H 'content-type: application/json' -d "{\"email\":\"$1\",\"password\":\"$2\"}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["accessToken"])'; }
ADMIN="Authorization: Bearer $(login admin@bistro.local BistroAdmin2026)"

curl -s -X POST $API/api/auth/register -H 'content-type: application/json' \
  -d '{"email":"ana@example.com","password":"Passw0rd-Demo1"}'      # 201; its accessToken goes in $ANA below
ANA="Authorization: Bearer <accessToken>"

curl -s -H "$ADMIN" -X POST $API/api/sittings \
  -H 'content-type: application/json' \
  -d '{"code":"FRI-DINNER","name":"Friday dinner","room":"Terrace","chef":"Chez Marlo","coversTotal":8,"maxPerGuest":4,"priceCents":8000}'

curl -s -H "$ANA" -X POST $API/api/orders \
  -H 'content-type: application/json' \
  -d '{"sittingCode":"FRI-DINNER","reference":"RSV-1001","covers":4}'
# 202, status Requested, totalCents 35840. Use the returned id. A fifth cover for the same account would be refused (limit 4):

curl -s -H "$ANA" $API/api/orders/<id>               # Sold after a few seconds, with a confirmation number
curl -s -H "$ANA" $API/api/confirmations/<id>
curl -s -H "$ANA" $API/api/sittings/FRI-DINNER        # coversLeft 4

# The sitting has 4 left. A party of 6 would be turned away, unless it asks to wait:
curl -s -H "$ANA" -X POST $API/api/orders -H 'content-type: application/json' \
  -d '{"sittingCode":"FRI-DINNER","reference":"RSV-1002","covers":6,"waitlist":true}'
# status Waitlisted, "number 1 in line". When the first party is refunded the 6 are seated by themselves.
curl -s -H "$ADMIN" $API/api/orders/<id>/events        # the Kafka log for this order

curl -s -H "$ANA" -X POST $API/api/orders/<id>/refund \
  -H 'content-type: application/json'                       # covers go back on sale; the confirmation stays
```

An order is validated at the door: guest reference required (at most 40 characters), 1 to 6 covers. A sitting's `maxPerGuest` (default 8, at most the covers in the sitting) caps what one account can hold in it; an order that would pass it is rejected with the reason `Cap` and writes no hold. Send `"waitlist": true` to wait for covers instead of being rejected when the sitting is full. The poison match is a case-sensitive substring: `RSV-POISON-7` and any longer token containing `POISON` match.

## Develop

```bash
make deps && make run                 # the API on :5231 against the local dependencies (migrates itself)
npx nx serve web                      # http://localhost:4311, proxies /api to :5231
WEB_URL=http://localhost:4311 npx playwright test
make lint                             # gofmt and go vet
make logs                             # follow the API logs of every replica in the cluster
make cluster-status
```

## What is tested, and where it runs

- **Unit tests** (no Docker, under a second each): the order rules and the wording of the refusals (a waitlisted order says its number in line), password hashing and policy, RFC 6238 test vectors for
  two-factor, access tokens (round trip, tampering, expiry inside and beyond the skew, `alg=none`, a token signed with another
  key, issuer and audience, RS256 and the algorithm confusion attack, the JWKS), one-time email tokens, the retry policy and
  the poison rule, the wire names of the messages and of the Kafka envelope, the problem+json responses, the correlation id and
  security headers, owner-or-staff access, config validation.
- **Integration tests** (`internal/integration`, a real Postgres, the real middleware chain and routes, brokers replaced by a
  loop that hands each outbox row to the handlers its routing key is bound to): register, login, lockout, refresh rotation,
  replay revoking the family, concurrent refreshes (exactly one wins), logout, change password, reset, email confirmation, the
  full two-factor lifecycle, the shared rate limit, RS256 and the JWKS, every route's 401/403/404 and ownership, paging,
  validation at the door; the sittings module (**concurrent reservations never oversell**, one guest racing themselves cannot pass the cap, a refused try
  costs neither covers nor allowance, a release gives both back exactly once) and the **waitlist** (a full sitting refuses
  without the flag and queues with it; a line exists, so a new reservation waits even when its covers would fit; releases
  promote strictly in order; the head of the line blocks smaller parties behind it; leaving the line removes the place and holds
  nothing; **a refund and a leave racing for the same covers never overbook, five rounds**; a promotion past the guest limit is
  refused as `Cap` and the covers move on); the whole saga (order to sold and a confirmation, the cap with the allowance coming
  back on a refund, poison dead-lettered and the covers released, a refund after the sale, a late final mark that loses to a
  refund, a repeated delivery that has no second effect, a final mark that arrives before the held mark and is retried); the outbox (several relays drain disjoint batches, a failed publish rolls the batch
  back, trimming keeps the inbox longer than the outbox) and migrations (six replicas migrating at once apply each file once,
  a broken file rolls back). Both suites run with the race detector in CI.
- **`scripts/smoke.sh`** drives the cluster over HTTP: authentication and ownership first (401, 403, 404), a password reset
  that ends the old sessions, validation, the happy path, the last cover with concurrent guests, the per-guest limit (a concurrent burst from one account), the waitlist (queued as number 1 and 2, an unflagged request rejected, a refund seats both in order), the poison reference (`confirmations.work.dlq` gains one message, covers and allowance restored), the dead letters listed and replayed, and a refund that lands between
  the document and the final mark.
- **Playwright** (`e2e/specs`, 16 specs, Chromium, against the web port of the cluster): sign-in and its refusals, the
  password policy, the session surviving a reload through the cookie, a password reset through the emailed link, two-factor
  sign-in, "sign out everywhere", the API refusing bad input and the page saying why, a sitting opened and booked and refunded in the UI, the last cover going to one request while the same guest asking again is held to the limit, a guest who cannot go past the limit and gets the allowance back on a refund, **a full sitting that lets a guest join the waitlist and a cancellation that seats them**, a poison order that is rejected, the Kafka event log.
- **`scripts/resilience.sh`** is the one only Kubernetes makes possible: it kills an API replica and the RabbitMQ pod in the middle of a booking, takes a third party while the broker is down, and checks afterwards that two parties were seated and confirmed, the third waits in the line, all four covers are held and no more, and that when a seat is given back the party in the line is seated exactly once.
- **CI** (`.github/workflows/ci.yml`) has two jobs. `verify`: gofmt and `go vet`, unit and integration tests with `-race`
  (Postgres 17 service container), `govulncheck`, `npm audit`, Prettier, the Angular type check, lint and build, and the
  Kustomize overlay rendering. `e2e`: creates a kind cluster, runs `make cluster-up`, the smoke script, the browser specs and
  the resilience script, and uploads cluster state, logs and traces on failure. Dependabot watches Go modules, npm, Actions and
  Docker.

## API

JSON over HTTP, the same as dotnet-bistro's. Everything except health and `/api/auth/register|login|refresh|logout` and the
email flows needs a bearer token.

| Method and path | What it does | Responses |
| --- | --- | --- |
| `GET /api/health`, `GET /api/health/ready` | Liveness; readiness (every module schema reachable). | `200`, `503` |
| `POST /api/auth/register` | Creates a customer and mails a confirmation link. Signs them in unless confirmation is required. | `201` signed in, `202` check your inbox, `400` policy, `409` email taken, `429` |
| `POST /api/auth/login` | Signs in (with `code` when two-factor is on); sets the refresh cookie. | `200`, `401`, `403` unconfirmed, `429` locked or rate limited |
| `POST /api/auth/refresh`, `POST /api/auth/logout` | Rotate or revoke the refresh cookie. Need `X-Requested-With: bistro-web`. | `200`/`204`, `400` header missing, `401` |
| `POST /api/auth/confirm-email`, `resend-confirmation` | Confirm an address with the emailed token; ask for a new link (always `202`). | `204`/`202`, `400` |
| `POST /api/auth/forgot-password`, `reset-password` | Mail a reset link (always `202`); set a new password with it (ends every session). | `202`/`204`, `400` |
| `POST /api/auth/change-password`, `logout-everywhere` | Signed in: change the password (ends the other sessions and returns a fresh one); end every session on every device. | `200`/`204`, `400`, `401` |
| `GET /api/auth/mfa`, `POST /api/auth/mfa/setup`, `enable`, `disable` | Two-factor status and lifecycle. | `200`/`204`, `400`, `409` |
| `GET /.well-known/jwks.json` | The public key set when RS256 is on. | `200`, `404` |
| `GET /api/auth/me` | The caller. | `200`, `401` |
| `POST /api/sittings` | Opens a sitting with its covers on sale, a price in cents and an optional `maxPerGuest` (default 8). Staff. | `201`, `400` invalid, `403`, `409` code exists |
| `GET /api/sittings`, `GET /api/sittings/{code}` | Sittings with the live covers left. | `200`, `404` |
| `POST /api/orders` | Accepts an order (1 to 6 covers) for the caller and starts the saga. | `202` Requested, `400` invalid, `404` unknown sitting, `409` duplicate reference (a full sitting or an over-limit guest is rejected, or waitlisted, later, by the saga) |
| `GET /api/orders?sittingCode=&page=&pageSize=` | The caller's orders, newest first, a page at a time (`pageSize` at most 100, default 100). Staff: everyone's. The total is in `X-Total-Count`. | `200` |
| `GET /api/orders/{id}` | One order and its status. Owner or staff. | `200`, `404` |
| `POST /api/orders/{id}/refund` | Moves a waitlisted, held or sold order to `Refunded`, puts the covers back (or removes the place in the line) and seats whoever is next. The confirmation is not touched. Owner or staff. | `202`, `404`, `409` wrong status |
| `GET /api/orders/{id}/events` | The Kafka event log for one order. Staff. | `200`, `403` |
| `GET /api/admin/dead-letters`, `POST /api/admin/dead-letters/{queue}/replay` | What waits in each dead-letter queue, with the first messages; publish them again. Staff. A message that still fails for the same reason is dead-lettered again. | `200`, `403`, `503` brokers off |
| `GET /api/confirmations/{orderId}` | The confirmation row. Owner or staff. | `200`, `404` |

Errors come back as `application/problem+json` with a `title` and the `correlationId`.

## Configuration

The process is configured from the environment (the ConfigMap and Secret in the cluster). Defaults work against `make deps`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://bistro:bistro@localhost:5491/bistro?sslmode=disable` | The one Postgres database; each module has its own schema. |
| `RABBITMQ_URL` / `KAFKA_BROKERS` | `amqp://platform:platform@localhost:5721/` / `localhost:9141` | Work queue and event log. |
| `MESSAGING_ENABLED` | `true` | Switch the relay and the consumers off (the integration tests do). |
| `JWT_KEY` | none | HS256 signing key, at least 32 bytes. Required; the process refuses to start without it (or `JWT_RSA_PRIVATE_KEY_PEM`). |
| `JWT_ACCESS_MINUTES` / `JWT_REFRESH_DAYS` / `JWT_SECURE_COOKIES` | `15` / `14` / `false` | Token lifetimes; set `JWT_SECURE_COOKIES` when TLS ends in front of the API. |
| `JWT_RSA_PRIVATE_KEY_PEM` / `JWT_KEY_ID` | unset / `bistro-1` | Sign with RS256 and publish the public key at `/.well-known/jwks.json`. |
| `JWT_REVOCATION_CACHE_SECONDS` | `5` (`2` in the demo) | How long a replica trusts a cached session version before re-reading it. |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | unset | The administrator created on first migration. |
| `AUTH_REQUIRE_CONFIRMED_EMAIL` / `AUTH_PUBLIC_URL` | `true` / `http://localhost:4311` | Sign-in waits for email confirmation; the address links in emails point to. |
| `MAIL_HOST` / `MAIL_PORT` / `MAIL_USER` / `MAIL_PASSWORD` / `MAIL_FROM` / `MAIL_START_TLS` | unset | SMTP for confirmation and reset emails. Unset means the log only. |
| `MAIL_DEV_MAILBOX` | `false` (`true` in the demo) | Keep the last messages in Postgres and serve them at `/api/dev/mailbox`. Never on in production. |
| `RATE_LIMIT_AUTH_PER_MINUTE` | `20` (`1000` in the demo) | Requests a minute per client to `/api/auth/*`. |
| `FORWARDED_HEADERS` | `false` (`true` behind nginx) | Trust `X-Forwarded-For` and `-Proto` from the reverse proxy. |
| `OUTBOX_RETENTION_DAYS` / `INBOX_RETENTION_DAYS` | `7` / `30` | Sent outbox rows and old inbox confirmations are deleted after this long. The inbox is kept longer because it must outlive any redelivery. |
| `ORDERS_FINAL_MARK_DELAY_MS` | `0` (`1500` in the demo) | Milliseconds the final mark waits. A latency knob so the smoke script can land a refund inside the window. |
| `MIGRATE_ON_START` | `true` | `false` in the cluster, where the init container migrates. |
| `LISTEN_ADDR` / `LOG_JSON` | `:8080` / `false` | |
| `TEST_DATABASE_URL` (tests only) | `postgres://bistro:bistro@localhost:5491/bistro_test?sslmode=disable` | The Postgres the integration tests use; the database is created if it is missing. |

## Look inside and fix things

```bash
make cluster-status                                   # pods, services, disruption budgets
kubectl -n bistro logs -f deploy/api --all-containers   # JSON logs (one line per request, one per worker event)
kubectl -n bistro exec postgres-0 -- psql -U bistro -d bistro -c 'select routing_key, sent_at from orders.outbox_messages order by id desc limit 10'
kubectl -n bistro delete pod -l app.kubernetes.io/name=api --wait=false   # a replica dies; the Service keeps answering
open http://127.0.0.1:15721                           # RabbitMQ management, platform / platform
```

- **Dead-lettered messages** sit in `confirmations.work.dlq` (and the two other `.dlq` queues). Staff can list and replay them from
  `/api/admin/dead-letters`, or inspect them in the management UI. Retries wait in `<queue>.retry` for 2 seconds.
- **A port is already in use**: `lsof -nP -i :8111`. The kind cluster and `make deps` both want RabbitMQ's management port
  (15721); stop one before starting the other.
- **Start clean**: `make cluster-down && make cluster-up` (the PVCs go with the cluster) and `make deps-down` for the
  dependencies.
- **A pod is not Ready after `make cluster-up`**: `kubectl -n bistro describe pod <name>` and the logs of the `migrate` init
  container, which is where a wrong database password shows up first.

## Known limits

- It runs on **kind**, a one-node cluster on your machine. Nothing here is a cloud deployment: there is no ingress controller,
  no TLS, no autoscaler (the HPA needs a metrics server) and one broker of each kind. The manifests are plain Kubernetes and
  Kustomize, and the stateful services are the part you would replace with managed ones.
- The waitlist is strict first come, first served: a large party at the head of the line holds up smaller parties behind it
  until enough covers return. Nobody is notified when they are seated; the UI and the API show the new status.
- The cap is per account. Nothing stops one person registering a second account, so the limit is only as hard as the account
  is to duplicate.
- The covers left are one row, and a promotion runs inside the release transaction, so a very long line is seated in one
  transaction. Fine for a dining room, not for a stadium.
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
internal/orders      order rules, the saga's first step and the event handler   (schema orders)
internal/sittings    the sitting, the cap, the covers, the waitlist     (schema sittings)
internal/confirmationsconfirmations                                       (schema confirmations)
internal/platform  config, pgdb (pool, migrations), httpx, messaging (outbox, relay, consumers, Kafka log, dead letters)
internal/integration  the integration suite     internal/testutil  the shared test database
apps/web           Angular 21, styled with Tailwind 4 utilities only (shared with dotnet-bistro)
e2e                Playwright specs
deploy/k8s         Kustomize base and the kind overlay       deploy/kind  the cluster     deploy/compose.deps.yaml  local dependencies
scripts            smoke.sh, resilience.sh, kind-secrets.sh
Dockerfile         two targets: api (distroless) and web (nginx)
```

## Other systems in this series

- [dotnet-bistro](https://github.com/joshbarros/dotnet-bistro): this system on ASP.NET Core and Docker Compose, with the same API and UI.
- [golang-sole](https://github.com/joshbarros/golang-sole): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-mint](https://github.com/joshbarros/golang-mint): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-pledge](https://github.com/joshbarros/golang-pledge): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-flash](https://github.com/joshbarros/golang-flash): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-encore](https://github.com/joshbarros/golang-encore): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-atelier](https://github.com/joshbarros/golang-atelier): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-freight](https://github.com/joshbarros/golang-freight): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-clinic](https://github.com/joshbarros/golang-clinic): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-warehouse](https://github.com/joshbarros/golang-warehouse): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-academy](https://github.com/joshbarros/golang-academy): the same kind of system for a different scarce number, also Go on Kubernetes.
- The `dotnet-*` series (freight, clinic, ledger, warehouse, academy, compute, hotel, tickets, carbon, charging, promo, mint, pledge, bistro, flash, encore, atelier, sole) has the whole family on ASP.NET Core; each adds one more way to keep a scarce number honest.
