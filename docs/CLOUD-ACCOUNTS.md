# Cloud accounts

A Bask account that is not an account on anybody's server, so one person can be
invited to another person's library without being given a credential on that
person's machine — the thing plex.tv does, in the smallest shape that does it.

This document is the protocol. The service lives in `cloud/`, deployed as a
Cloudflare Worker with a D1 database; BMS verifies what it signs.

## What it is not

**BMS does not need it.** A server with no cloud configured works exactly as it
always has: local accounts, local tokens, nothing phones anywhere. Every
sentence below describes something additional, and a household that never wants
an account service never gets one. If the service is down, a client that has
already been admitted keeps working against the addresses it knows.

**It is not a relay.** Plex's account service is half of plex.tv; the other
half is NAT traversal and a fleet of relay servers, and that half is the
expensive one. This service tells a client the addresses of the servers it may
reach, and reaching them is the operator's problem — a tailnet, a tunnel, a
port forward. Nothing here proxies a byte of video.

**It is not an identity provider for anything else.** It issues one kind of
assertion, to one kind of audience, and the only software that verifies one is
a Bask server.

## The shape

```
  a client                 the service                 a server
  (phone, TV)              (Worker + D1)               (BMS)

  link a device  ───────►  code on screen
                           approved in a browser
                 ◄───────  a client token

  which servers? ────────► membership
                 ◄───────  addresses + a signed assertion

  the assertion  ─────────────────────────────────────►  verify the signature
                                                         map to a local account
                                                         apply library grants
```

Three nouns:

- **An account** is a person: an email address and a way to prove they own it.
- **A server** is one BMS, claimed by the account that administers it.
- **A membership** is an account allowed on a server, made by an invitation
  from that server's owner.

## Linking a device

A television has no keyboard worth the name, so a device is linked the way a
television should be: a short code on the screen, typed into a phone.

```
POST /link/start        {client, device}
  → {user_code, verification_uri, device_code, interval, expires_in}
POST /link/poll         {device_code}
  → 428 while nobody has approved it, then {client_token, account}
```

The `user_code` is eight characters from an alphabet with no `0`, `O`, `1`, `I`
or `5` in it, in two groups of four, because it is read off a television across
a room and typed by somebody who is annoyed. It is good for ten minutes and
dies on first use.

`client_token` is long-lived and belongs to that one device, stored hashed,
revocable on its own — the same discipline BMS uses for its own tokens, for the
same reason.

Polling is rate limited, and `interval` is what the client is told to wait.
A client that polls faster is told to slow down rather than refused, because a
client that gets an error during a sign-in shows the viewer a failure.

## Claiming a server

An administrator signs in to the cloud from the BMS admin interface and claims
the server they are administering:

```
POST /servers           {name}                     (as an account)
  → {server_id, server_secret}
PUT  /servers/{id}/addresses  {addresses: [...]}   (as a server)
```

`server_secret` is how the server authenticates afterwards; it is shown once
and stored hashed, and BMS keeps its copy where it keeps nothing else — the
server table, not the library.

The addresses are whatever reaches that server: a tailnet name, a tunnel
hostname, a bare address and port. The service stores them and hands them out;
it never connects to them, so it never learns whether they work. A client tries
them in order and remembers which one answered.

## Inviting somebody

```
POST /servers/{id}/invitations  {email}      (as the owner)
  → {code}
POST /invitations/{code}/accept              (as the invited account)
  → {membership}
DELETE /servers/{id}/members/{account}       (as the owner)
```

An invitation names an email address and is good for seven days. Accepting it
creates a membership, which is what makes that server appear in that account's
list. Revoking it removes the membership, and the next assertion the invitee
asks for is refused — but a client already holding a valid assertion keeps
working until it expires, which is why assertions are short.

**The invitation does not grant access to anything.** It makes the account
known to the server. Which libraries that person then sees is the server
administrator's decision, set in the BMS admin interface with the same
per-account library grants every local account has. The cloud never learns what
is in anybody's library, and it is not consulted when a request is authorised.

## The assertion

What a client presents to a server, and the only thing a server trusts the
service for.

```
POST /servers/{id}/assertion    (as a client token)
  → {assertion, expires_in}
```

It is a compact JWS, `EdDSA` over Ed25519:

```
base64url(header) . base64url(payload) . base64url(signature)

header   {"alg":"EdDSA","typ":"bask+jws","kid":"…"}
payload  {"iss":"https://<service>", "sub":"<account id>",
          "aud":"<server id>", "email":"…", "name":"…",
          "iat":…, "exp":…, "jti":"…"}
```

Five minutes, because the only thing it has to survive is being handed over.
A client asks for a new one when the old one is nearly out, and a server that
refuses an expired one costs that client one round trip.

JWS rather than something of our own because the shape is well understood and
the verification is forty lines of Go against `crypto/ed25519`, with no
dependency. The algorithm is not read from the header: a server verifies
`EdDSA` and nothing else, so `alg: none` and the rest of that family of
mistakes are not available.

### What a server checks

In this order, and all of them:

1. The signature, against the key named by `kid`.
2. `iss` is the service this server was claimed with.
3. `aud` is this server's own id. An assertion for somebody else's server is
   not a credential for this one, and this is the check that makes a stolen
   assertion worthless elsewhere.
4. `exp` is in the future and `iat` is not far in it, against a small skew.
5. `jti` has not been seen inside the lifetime window. Replay inside five
   minutes is otherwise free, and the window is small enough that remembering
   every one of them costs nothing.

Then `sub` is mapped to a local account: the first time, one is created,
inactive, named from the assertion, and the administrator is told somebody is
waiting. Afterwards it is the same local account every time, with whatever
library grants it has been given. **A cloud identity never arrives as an
administrator**, and it never changes an existing local account's grants.

### Keys

The service publishes its public keys and nothing else at

```
GET /.well-known/bask-keys.json
  → {"keys":[{"kid":"…","alg":"EdDSA","key":"<base64url Ed25519>"}]}
```

A server fetches them when it claims, pins what it saw, and refreshes on a
`kid` it does not know — no more often, so an unreachable service does not
stop a server verifying the assertions it already understands. Rotation
publishes the new key alongside the old one for longer than an assertion can
live, so there is never a moment when a valid assertion cannot be checked.

The private keys live in Workers Secrets Store and are never in the database,
in the repository, or in a log.

## On the server

What the administrator does, and what a client presents.

### Claiming, from the admin interface

Settings carries a Bask account card. The administrator signs in to the service
in a browser, asks it for a claim token, and pastes that in; the server redeems
it, fetches the published keys and pins them, and stores the whole link in the
`server` table under `cloud.link`. The secret is in that row and in nothing the
interface ever serves — the card shows the issuer, the server id and when it was
claimed, and no more.

A claim that cannot fetch the keys fails and stores nothing. A link with a
secret and no keys would look claimed while verifying nothing a client
presented, which is worse than being unclaimed.

The same card publishes the addresses this server can be reached at, and
unlinks. Unlinking leaves the local accounts a cloud identity created: they hold
library grants somebody made by hand, and removing them would quietly undo that
work. Nothing can authenticate as them while there is no link.

### Presenting an assertion

```
X-Bask-Assertion: <compact JWS>
```

Also accepted in `X-Plex-Token`, in the header or the query string, because a
client that speaks this dialect already sends that everywhere and a stream URL
can carry nothing else. The two cannot be confused: an assertion has two dots
in it and a token this server issued has none. A server that has not been
claimed ignores the header entirely and its own tokens are untouched.

### Becoming a local account

A verified `sub` is looked up in `users.cloud_subject`. The first time one
turns up, an account is created for it — never an administrator, never active —
named from the assertion, and the request is refused with **403** rather than
401: the identity is good, signing in again would not help, and a client told
401 would loop asking the service for another assertion. The account appears on
the Users page marked *invited* and *waiting*, with an Allow button, and the
Settings card counts how many are.

Once allowed it reaches exactly the libraries that account was granted, read
through the same path every other credential uses. There is no token row, so
there is no device grant to combine and nothing to revoke per device; revoking
is done at the service, or by suspending the account here.

`users.active` applies to every account, not only to these. An account that is
not active authenticates as nothing whatever it is holding — suspending somebody
and leaving their television working would not be suspending them.

## What the service stores

Six tables, and deliberately nothing else:

| Table | What |
|---|---|
| `accounts` | id, email, password hash, display name, created |
| `clients` | id, account, label, token hash, last seen |
| `servers` | id, owner account, name, secret hash, created |
| `addresses` | server, address, order |
| `memberships` | server, account, created |
| `invitations` | code hash, server, email, expiry, accepted |

No library names, no titles, no viewing history, no addresses of clients. The
service knows that an account may reach a server; it does not know what is on
it or what anybody watched. That is not a courtesy — it is what keeps this
thing small enough to run for nothing and dull enough to be worth running.

Passwords are PBKDF2-HMAC-SHA256 at 600,000 iterations with a per-account salt,
which is what the Workers runtime can do natively and what OWASP asks for when
it is what you have. Tokens, secrets and invitation codes are 32 bytes of
`crypto.getRandomValues`, stored only as SHA-256.

## Rate limits

Every unauthenticated route is limited per address, and the sign-in and
link-approval routes are limited per account as well, because the second one is
what stops a code being guessed. A limit answers `429` with `Retry-After`, and
never a different answer depending on whether an email address exists.

## Why a Worker

It is already paid for, it has a database, it is reachable from everywhere
without anything being opened on a home network, and it costs nothing at a
household's scale. The alternative — running the account service on the same
machine as the library — is the thing this is for: a friend's client has to
reach *something* to find out where your server is, and it should not have to
reach your server to do it.
