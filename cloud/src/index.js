// The Bask account service.
//
// One Worker, one D1 database, and the smallest set of routes that lets one
// person be invited to another person's library without being handed a
// credential on that person's machine. The protocol, and what is deliberately
// absent from it, is docs/CLOUD-ACCOUNTS.md.
//
// Nothing here knows what is in anybody's library. The service says that an
// account may reach a server and signs a short assertion saying who they are;
// the server decides what they then see, with the same per-account library
// grants every local account has.

import {
  base64url,
  derive,
  hash,
  id,
  normaliseUserCode,
  publicKeyOf,
  random,
  sameSecret,
  signAssertion,
  userCode,
} from "./crypto.js";
import {
  bearer,
  clientAddress,
  fail,
  foldEmail,
  json,
  limit,
  looksLikeEmail,
  now,
  readJSON,
  tooMany,
} from "./http.js";

// How long each kind of secret is good for, in seconds.
const LINK_LIFETIME = 10 * 60;
const INVITATION_LIFETIME = 7 * 24 * 60 * 60;
// An assertion only has to survive being handed over, so it is short. A
// revoked membership stops working within this, which is the number that
// decides how wrong a revocation can be.
const ASSERTION_LIFETIME = 5 * 60;
// What a polling client is told to wait between attempts.
const POLL_INTERVAL = 5;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    try {
      return await route(request, env, url);
    } catch (err) {
      // Never the error itself: a stack trace in an answer tells a stranger
      // about the inside of this service, and the one person who can act on
      // it is reading the log instead.
      console.error("unhandled", url.pathname, err && err.stack ? err.stack : err);
      return fail(500, "something went wrong");
    }
  },
};

async function route(request, env, url) {
  const path = url.pathname.replace(/\/+$/, "") || "/";
  const method = request.method.toUpperCase();

  if (method === "GET" && path === "/.well-known/bask-keys.json") return keys(env);
  if (method === "GET" && path === "/health") return json({ ok: true });

  if (method === "POST" && path === "/accounts") return register(request, env);
  if (method === "POST" && path === "/sessions") return signIn(request, env);

  if (method === "POST" && path === "/link/start") return linkStart(request, env);
  if (method === "POST" && path === "/link/poll") return linkPoll(request, env);
  if (method === "POST" && path === "/link/approve") return linkApprove(request, env);

  if (method === "GET" && path === "/me") return me(request, env);
  if (method === "GET" && path === "/me/servers") return myServers(request, env);

  if (method === "POST" && path === "/servers") return claimServer(request, env);

  const server = path.match(/^\/servers\/([A-Za-z0-9_-]{1,64})(\/[A-Za-z0-9_\-/]*)?$/);
  if (server) {
    const [, serverID, rest = ""] = server;
    if (method === "PUT" && rest === "/addresses") return setAddresses(request, env, serverID);
    if (method === "POST" && rest === "/invitations") return invite(request, env, serverID);
    if (method === "GET" && rest === "/members") return members(request, env, serverID);
    if (method === "POST" && rest === "/assertion") return assertion(request, env, serverID);
    const member = rest.match(/^\/members\/([A-Za-z0-9_-]{1,64})$/);
    if (method === "DELETE" && member) return removeMember(request, env, serverID, member[1]);
  }

  const invitation = path.match(/^\/invitations\/([A-Za-z0-9_-]{1,128})\/accept$/);
  if (method === "POST" && invitation) return acceptInvitation(request, env, invitation[1]);

  return fail(404, "no such endpoint");
}

/* ---------- keys ---------- */

/**
 * keys publishes the public halves, and nothing else.
 *
 * Both the current key and the one before it while a rotation is in flight,
 * for longer than an assertion can live, so there is never a moment when a
 * valid assertion cannot be checked.
 */
async function keys(env) {
  const published = [];
  for (const [kid, secret] of signingKeys(env)) {
    published.push({ kid, alg: "EdDSA", key: await publicKeyOf(secret) });
  }
  if (published.length === 0) return fail(503, "this service has no signing key configured");
  return json({ keys: published }, 200, {
    // Long enough that a server is not refetching constantly, short enough
    // that a rotation reaches everybody inside a day.
    "cache-control": "public, max-age=3600",
  });
}

/**
 * signingKeys is the current key first, then any still being retired.
 *
 * A rotation sets SIGNING_KEY to the new one and moves the old value to
 * SIGNING_KEY_PREVIOUS, where it stays long enough for every assertion signed
 * with it to expire. Signing always uses the first.
 */
function signingKeys(env) {
  const out = [];
  if (env.SIGNING_KEY) out.push([env.SIGNING_KEY_ID || "current", env.SIGNING_KEY]);
  if (env.SIGNING_KEY_PREVIOUS) {
    out.push([env.SIGNING_KEY_PREVIOUS_ID || "previous", env.SIGNING_KEY_PREVIOUS]);
  }
  return out;
}

/* ---------- accounts ---------- */

async function register(request, env) {
  if (!(await limit(env, "register:" + clientAddress(request)))) return tooMany();

  const { value: body, error } = await readJSON(request);
  if (error) return error;

  const email = String(body.email || "").trim();
  const password = String(body.password || "");
  const name = String(body.name || "").trim().slice(0, 64);
  if (!looksLikeEmail(email)) return fail(400, "that does not look like an email address");
  if (password.length < 10) return fail(400, "a password needs at least ten characters");
  if (password.length > 512) return fail(400, "that password is unreasonably long");

  const folded = foldEmail(email);
  const salt = random(16);
  const accountID = id();
  const derived = await derive(password, salt);

  // The uniqueness is the database's to enforce, not ours to check first:
  // checking and then inserting is two steps that two requests can interleave.
  const inserted = await env.DB.prepare(
    `INSERT INTO accounts (id, email, email_folded, name, password_hash, password_salt, created_at)
     VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(email_folded) DO NOTHING`,
  )
    .bind(accountID, email, folded, name, derived, salt, now())
    .run();

  if (!inserted.meta.changes) {
    // Deliberately the same answer as a successful registration would give a
    // stranger nothing: this route tells you an address is taken, because a
    // sign-up form that silently does nothing is worse than useless and the
    // address is one somebody is claiming to own anyway.
    return fail(409, "there is already an account with that address");
  }
  return json({ account: { id: accountID, email, name } }, 201);
}

/**
 * signIn exchanges an email and password for a client token.
 *
 * The same route a browser uses to approve a link, so the token it returns is
 * an ordinary client token belonging to a device called "browser".
 */
async function signIn(request, env) {
  const address = clientAddress(request);
  if (!(await limit(env, "signin:" + address))) return tooMany();

  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const folded = foldEmail(body.email);
  const password = String(body.password || "");
  const label = String(body.device || "browser").trim().slice(0, 64);

  const account = await env.DB.prepare(
    `SELECT id, email, name, password_hash, password_salt FROM accounts WHERE email_folded = ?`,
  )
    .bind(folded)
    .first();

  // The work happens either way. Answering a missing address faster than a
  // wrong password is how an attacker enumerates which addresses exist.
  const salt = account ? account.password_salt : random(16);
  const derived = await derive(password, salt);
  if (!account || !sameSecret(derived, account.password_hash)) {
    if (!(await limit(env, "signin-fail:" + folded))) return tooMany();
    return fail(401, "that email address and password do not match");
  }

  const token = await issueClient(env, account.id, label);
  return json({ token, account: { id: account.id, email: account.email, name: account.name } });
}

async function issueClient(env, accountID, label) {
  const token = random(32);
  await env.DB.prepare(
    `INSERT INTO clients (id, account_id, label, token_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
  )
    .bind(id(), accountID, label, await hash(token), now())
    .run();
  return token;
}

/**
 * accountFor resolves a client token, or null.
 *
 * last_seen_at is updated on a best-effort basis: it is diagnostics, and a
 * failed write must not refuse a valid request.
 */
async function accountFor(request, env) {
  const token = bearer(request);
  if (!token) return null;
  const digest = await hash(token);
  const row = await env.DB.prepare(
    `SELECT c.id AS client_id, a.id, a.email, a.name
       FROM clients c JOIN accounts a ON a.id = c.account_id
      WHERE c.token_hash = ?`,
  )
    .bind(digest)
    .first();
  if (!row) return null;
  env.DB.prepare(`UPDATE clients SET last_seen_at = ? WHERE id = ?`)
    .bind(now(), row.client_id)
    .run()
    .catch(() => {});
  return { id: row.id, email: row.email, name: row.name, clientID: row.client_id };
}

async function me(request, env) {
  const account = await accountFor(request, env);
  if (!account) return fail(401, "sign in first");
  return json({ account: { id: account.id, email: account.email, name: account.name } });
}

/* ---------- linking a device ---------- */

/**
 * linkStart is what a television asks for before it shows a code.
 *
 * The device gets a secret only it holds and the viewer gets eight characters
 * to type somewhere comfortable. Neither is the other: knowing the code shown
 * on a screen must not let a stranger collect the token.
 */
async function linkStart(request, env) {
  if (!(await limit(env, "link:" + clientAddress(request)))) return tooMany();

  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const client = String(body.client || "").trim().slice(0, 64);
  const device = String(body.device || "").trim().slice(0, 64);

  const deviceCode = random(32);
  const code = userCode();
  await env.DB.prepare(
    `INSERT INTO link_requests (device_hash, user_code, client_name, device_name, expires_at, created_at)
     VALUES (?, ?, ?, ?, ?, ?)`,
  )
    .bind(await hash(deviceCode), code, client, device, now() + LINK_LIFETIME, now())
    .run();

  return json({
    user_code: code,
    device_code: deviceCode,
    verification_uri: (env.PUBLIC_ORIGIN || "") + "/link",
    interval: POLL_INTERVAL,
    expires_in: LINK_LIFETIME,
  });
}

/**
 * linkPoll is the television asking whether anybody has approved it yet.
 *
 * 428 while it waits, because a client that is told "error" during a sign-in
 * shows the viewer a failure, and this is not one.
 */
async function linkPoll(request, env) {
  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const deviceCode = String(body.device_code || "");
  if (!deviceCode) return fail(400, "that request needs a device code");

  const digest = await hash(deviceCode);
  const row = await env.DB.prepare(
    `SELECT device_hash, account_id, approved_at, expires_at, client_name, device_name
       FROM link_requests WHERE device_hash = ?`,
  )
    .bind(digest)
    .first();
  if (!row) return fail(404, "that link request is not one this service knows");
  if (row.expires_at < now()) {
    await env.DB.prepare(`DELETE FROM link_requests WHERE device_hash = ?`).bind(digest).run();
    return fail(410, "that code has expired; ask for another");
  }
  if (!row.approved_at) {
    return json({ status: "pending", interval: POLL_INTERVAL }, 428, {
      "retry-after": String(POLL_INTERVAL),
    });
  }

  // Approved. The request is spent whether or not the token reaches the
  // client, because a link request that can be redeemed twice is a code
  // somebody can use after the television already has.
  await env.DB.prepare(`DELETE FROM link_requests WHERE device_hash = ?`).bind(digest).run();

  const label = [row.device_name, row.client_name].filter(Boolean).join(" · ") || "a device";
  const token = await issueClient(env, row.account_id, label);
  const account = await env.DB.prepare(`SELECT id, email, name FROM accounts WHERE id = ?`)
    .bind(row.account_id)
    .first();
  return json({ status: "linked", token, account });
}

/** linkApprove is the phone, signed in, saying yes to what is on the screen. */
async function linkApprove(request, env) {
  const account = await accountFor(request, env);
  if (!account) return fail(401, "sign in first");
  if (!(await limit(env, "approve:" + account.id))) return tooMany();

  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const code = normaliseUserCode(body.user_code);
  if (!code) return fail(400, "that is not one of this service's codes");

  const updated = await env.DB.prepare(
    `UPDATE link_requests SET account_id = ?, approved_at = ?
      WHERE user_code = ? AND approved_at = 0 AND expires_at >= ?`,
  )
    .bind(account.id, now(), code, now())
    .run();
  if (!updated.meta.changes) {
    return fail(404, "that code is not waiting for approval; it may have expired");
  }

  const waiting = await env.DB.prepare(
    `SELECT client_name, device_name FROM link_requests WHERE user_code = ?`,
  )
    .bind(code)
    .first();
  return json({ status: "approved", device: waiting ? waiting.device_name : "" });
}

/* ---------- servers ---------- */

/** claimServer is an administrator saying "this one is mine". */
async function claimServer(request, env) {
  const account = await accountFor(request, env);
  if (!account) return fail(401, "sign in first");

  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const name = String(body.name || "").trim().slice(0, 64) || "Bask Media Server";

  const serverID = id();
  const secret = random(32);
  await env.DB.batch([
    env.DB.prepare(
      `INSERT INTO servers (id, owner_id, name, secret_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
    ).bind(serverID, account.id, name, await hash(secret), now()),
    // The owner is a member of their own server, so one query answers "what
    // may this account reach" without a special case for the person who owns
    // the thing.
    env.DB.prepare(
      `INSERT INTO memberships (server_id, account_id, created_at) VALUES (?, ?, ?)`,
    ).bind(serverID, account.id, now()),
  ]);

  return json({ server: { id: serverID, name }, secret }, 201);
}

/** serverFor resolves a server secret, for the routes a server itself calls. */
async function serverFor(request, env, serverID) {
  const secret = bearer(request);
  if (!secret) return null;
  const row = await env.DB.prepare(
    `SELECT id, owner_id, name, secret_hash FROM servers WHERE id = ?`,
  )
    .bind(serverID)
    .first();
  if (!row || !sameSecret(await hash(secret), row.secret_hash)) return null;
  env.DB.prepare(`UPDATE servers SET seen_at = ? WHERE id = ?`)
    .bind(now(), serverID)
    .run()
    .catch(() => {});
  return row;
}

/** ownerOf resolves an account that owns this server, for the owner's routes. */
async function ownerOf(request, env, serverID) {
  const account = await accountFor(request, env);
  if (!account) return null;
  const row = await env.DB.prepare(`SELECT id, owner_id, name FROM servers WHERE id = ?`)
    .bind(serverID)
    .first();
  if (!row || row.owner_id !== account.id) return null;
  return { account, server: row };
}

/**
 * setAddresses is the server announcing where it can be reached.
 *
 * Called by the server with its own secret rather than by its owner, because
 * the server is the only party that knows when its address changed.
 */
async function setAddresses(request, env, serverID) {
  const server = await serverFor(request, env, serverID);
  if (!server) return fail(401, "that is not this server's secret");

  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const given = Array.isArray(body.addresses) ? body.addresses : [];
  if (given.length > 8) return fail(400, "that is more addresses than a server needs");

  const addresses = [];
  for (const entry of given) {
    const address = String(entry || "").trim();
    if (!address || address.length > 255) continue;
    // Stored as given and never fetched by this service, so the only thing
    // worth refusing is something that is not an address at all.
    if (/[\s<>"']/.test(address)) return fail(400, "that is not an address");
    if (!addresses.includes(address)) addresses.push(address);
  }

  const statements = [
    env.DB.prepare(`DELETE FROM addresses WHERE server_id = ?`).bind(serverID),
    ...addresses.map((address, position) =>
      env.DB.prepare(
        `INSERT INTO addresses (server_id, address, position) VALUES (?, ?, ?)`,
      ).bind(serverID, address, position),
    ),
  ];
  await env.DB.batch(statements);
  return json({ addresses });
}

/** myServers is what a client asks on startup: where may I go. */
async function myServers(request, env) {
  const account = await accountFor(request, env);
  if (!account) return fail(401, "sign in first");

  const { results } = await env.DB.prepare(
    `SELECT s.id, s.name, s.owner_id, a.address
       FROM memberships m
       JOIN servers s ON s.id = m.server_id
       LEFT JOIN addresses a ON a.server_id = s.id
      WHERE m.account_id = ?
      ORDER BY s.created_at, a.position`,
  )
    .bind(account.id)
    .all();

  const byID = new Map();
  for (const row of results || []) {
    if (!byID.has(row.id)) {
      byID.set(row.id, {
        id: row.id,
        name: row.name,
        owned: row.owner_id === account.id,
        addresses: [],
      });
    }
    if (row.address) byID.get(row.id).addresses.push(row.address);
  }
  return json({ servers: [...byID.values()] });
}

/* ---------- invitations ---------- */

async function invite(request, env, serverID) {
  const owner = await ownerOf(request, env, serverID);
  if (!owner) return fail(404, "no such server");

  const { value: body, error } = await readJSON(request);
  if (error) return error;
  const email = String(body.email || "").trim();
  if (!looksLikeEmail(email)) return fail(400, "that does not look like an email address");

  const code = random(32);
  await env.DB.prepare(
    `INSERT INTO invitations (code_hash, server_id, email_folded, expires_at, created_at)
     VALUES (?, ?, ?, ?, ?)`,
  )
    .bind(await hash(code), serverID, foldEmail(email), now() + INVITATION_LIFETIME, now())
    .run();

  // The service does not send the mail. Handing the owner the link and
  // letting them send it however they already talk to that person is one
  // fewer dependency, one fewer place a bounce goes unnoticed, and one fewer
  // way this becomes something that can send mail to strangers.
  return json(
    {
      invitation: {
        code,
        email,
        link: (env.PUBLIC_ORIGIN || "") + "/invite#" + code,
        expires_in: INVITATION_LIFETIME,
      },
    },
    201,
  );
}

async function acceptInvitation(request, env, code) {
  const account = await accountFor(request, env);
  if (!account) return fail(401, "sign in first");

  const digest = await hash(code);
  const row = await env.DB.prepare(
    `SELECT code_hash, server_id, email_folded, expires_at, accepted_at
       FROM invitations WHERE code_hash = ?`,
  )
    .bind(digest)
    .first();
  if (!row) return fail(404, "that invitation is not one this service knows");
  if (row.accepted_at) return fail(409, "that invitation has already been used");
  if (row.expires_at < now()) return fail(410, "that invitation has expired");
  // Addressed to somebody. An invitation that anybody holding the link can
  // redeem is a link, and a link forwarded once is a stranger on the server.
  if (row.email_folded !== foldEmail(account.email)) {
    return fail(403, "that invitation was sent to a different address");
  }

  await env.DB.batch([
    env.DB.prepare(
      `INSERT INTO memberships (server_id, account_id, created_at) VALUES (?, ?, ?)
       ON CONFLICT(server_id, account_id) DO NOTHING`,
    ).bind(row.server_id, account.id, now()),
    env.DB.prepare(`UPDATE invitations SET accepted_at = ? WHERE code_hash = ?`).bind(
      now(),
      digest,
    ),
  ]);

  const server = await env.DB.prepare(`SELECT id, name FROM servers WHERE id = ?`)
    .bind(row.server_id)
    .first();
  return json({ membership: { server } }, 201);
}

/**
 * members is the list a server reads to know who it should expect.
 *
 * Called by the server with its own secret: it is the party that needs to
 * know, and it needs to know without anybody being signed in.
 */
async function members(request, env, serverID) {
  const server = await serverFor(request, env, serverID);
  if (!server) return fail(401, "that is not this server's secret");

  const { results } = await env.DB.prepare(
    `SELECT a.id, a.email, a.name, m.created_at
       FROM memberships m JOIN accounts a ON a.id = m.account_id
      WHERE m.server_id = ? ORDER BY m.created_at`,
  )
    .bind(serverID)
    .all();
  return json({
    members: (results || []).map((row) => ({
      id: row.id,
      email: row.email,
      name: row.name,
      owner: row.id === server.owner_id,
      since: row.created_at,
    })),
  });
}

async function removeMember(request, env, serverID, accountID) {
  const owner = await ownerOf(request, env, serverID);
  if (!owner) return fail(404, "no such server");
  if (accountID === owner.server.owner_id) {
    return fail(409, "the owner cannot be removed from their own server");
  }

  const removed = await env.DB.prepare(
    `DELETE FROM memberships WHERE server_id = ? AND account_id = ?`,
  )
    .bind(serverID, accountID)
    .run();
  if (!removed.meta.changes) return fail(404, "that account is not a member");
  return new Response(null, { status: 204 });
}

/* ---------- assertions ---------- */

/**
 * assertion is the only thing a server trusts this service for.
 *
 * Short-lived and addressed to one server: an assertion for somebody else's
 * server is not a credential for this one, and that is the property that makes
 * a stolen one worthless anywhere but where it was meant to go.
 */
async function assertion(request, env, serverID) {
  const account = await accountFor(request, env);
  if (!account) return fail(401, "sign in first");

  const [current] = signingKeys(env);
  if (!current) return fail(503, "this service has no signing key configured");
  const [kid, secret] = current;

  const membership = await env.DB.prepare(
    `SELECT 1 AS present FROM memberships WHERE server_id = ? AND account_id = ?`,
  )
    .bind(serverID, account.id)
    .first();
  // The same answer as a server that does not exist, so holding a client
  // token is not a way to find out which servers this service knows about.
  if (!membership) return fail(404, "no such server");

  const issued = now();
  const token = await signAssertion(secret, kid, {
    iss: env.PUBLIC_ORIGIN || "",
    sub: account.id,
    aud: serverID,
    email: account.email,
    name: account.name,
    iat: issued,
    exp: issued + ASSERTION_LIFETIME,
    jti: base64url(crypto.getRandomValues(new Uint8Array(16))),
  });
  return json({ assertion: token, expires_in: ASSERTION_LIFETIME });
}
