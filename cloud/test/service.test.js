// What the account service promises, exercised against a real D1 inside
// workerd. The SQL and the crypto are the parts worth testing, and a mock of
// either would be a test of the mock.

import { env, createExecutionContext, waitOnExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import worker from "../src/index.js";
import schema from "../schema.sql?raw";

// A signing key generated once for the suite. Ed25519 private keys are cheap
// to make and this one never leaves the test process.
const signingKey = await (async () => {
  const pair = await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
  const pkcs8 = await crypto.subtle.exportKey("pkcs8", pair.privateKey);
  let binary = "";
  for (const byte of new Uint8Array(pkcs8)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
})();

function configured() {
  return {
    ...env,
    SIGNING_KEY: signingKey,
    SIGNING_KEY_ID: "k1",
    PUBLIC_ORIGIN: "https://account.example",
  };
}

// Each call comes from its own address unless a test says otherwise. The rate
// limiter is bound in this runtime and it is right that it is: a suite that
// shares one address is one client making hundreds of attempts, and it would
// spend its time being throttled instead of testing anything. The limiter gets
// a test of its own below.
let caller = 0;

async function call(method, path, { body, token, from } = {}) {
  const headers = { "cf-connecting-ip": from || `203.0.113.${++caller % 250}` };
  if (body !== undefined) headers["content-type"] = "application/json";
  if (token) headers.authorization = "Bearer " + token;
  const request = new Request("https://account.example" + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const ctx = createExecutionContext();
  const response = await worker.fetch(request, configured(), ctx);
  await waitOnExecutionContext(ctx);
  const text = await response.text();
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = { raw: text };
  }
  return { status: response.status, body: parsed, headers: response.headers };
}

/** account registers somebody and returns their client token. */
async function account(email, password = "a-long-enough-password") {
  const made = await call("POST", "/accounts", { body: { email, password, name: email } });
  expect(made.status).toBe(201);
  const session = await call("POST", "/sessions", { body: { email, password } });
  expect(session.status).toBe(200);
  return { token: session.body.token, id: session.body.account.id, email };
}

/** server claims one as this account and returns its id and secret. */
async function server(token, name = "The house") {
  const made = await call("POST", "/servers", { body: { name }, token });
  expect(made.status).toBe(201);
  return { id: made.body.server.id, secret: made.body.secret };
}

beforeEach(async () => {
  // A clean database per test. The schema is the file the deployment applies,
  // so a column that only exists in a test cannot happen.
  for (const statement of schema.split(";")) {
    const trimmed = statement.trim();
    if (trimmed) await env.DB.prepare(trimmed).run();
  }
  await env.DB.batch(
    ["invitations", "memberships", "addresses", "servers", "link_requests", "clients", "accounts"].map(
      (table) => env.DB.prepare(`DELETE FROM ${table}`),
    ),
  );
});

describe("accounts", () => {
  it("registers somebody and signs them back in", async () => {
    const made = await call("POST", "/accounts", {
      body: { email: "rae@example.com", password: "a-long-enough-password", name: "Rae" },
    });
    expect(made.status).toBe(201);

    const session = await call("POST", "/sessions", {
      body: { email: "RAE@example.com", password: "a-long-enough-password" },
    });
    expect(session.status).toBe(200);
    expect(session.body.account.email).toBe("rae@example.com");
    expect(session.body.token).toBeTruthy();
  });

  it("refuses a second account on the same address, whatever its case", async () => {
    await account("rae@example.com");
    const again = await call("POST", "/accounts", {
      body: { email: "Rae@Example.com", password: "another-long-password" },
    });
    expect(again.status).toBe(409);
  });

  it("refuses a short password rather than storing a weak one", async () => {
    const made = await call("POST", "/accounts", {
      body: { email: "rae@example.com", password: "short" },
    });
    expect(made.status).toBe(400);
  });

  it("answers a wrong password and an unknown address the same way", async () => {
    await account("rae@example.com");
    const wrong = await call("POST", "/sessions", {
      body: { email: "rae@example.com", password: "not-the-password" },
    });
    const missing = await call("POST", "/sessions", {
      body: { email: "nobody@example.com", password: "not-the-password" },
    });
    expect(wrong.status).toBe(401);
    expect(missing.status).toBe(401);
    expect(wrong.body.error).toBe(missing.body.error);
  });

  it("does not store the password", async () => {
    await account("rae@example.com");
    const row = await env.DB.prepare(`SELECT password_hash FROM accounts`).first();
    expect(row.password_hash).not.toContain("a-long-enough-password");
    expect(row.password_hash.length).toBeGreaterThan(20);
  });
});

describe("linking a device", () => {
  it("waits, is approved from a phone, and hands the television a token", async () => {
    const rae = await account("rae@example.com");

    const started = await call("POST", "/link/start", {
      body: { client: "Bask", device: "Living room TV" },
    });
    expect(started.status).toBe(200);
    expect(started.body.user_code).toMatch(/^[A-Z2-9]{4}-[A-Z2-9]{4}$/);

    const waiting = await call("POST", "/link/poll", {
      body: { device_code: started.body.device_code },
    });
    expect(waiting.status).toBe(428);

    // Typed by somebody in a hurry: lower case, no separator.
    const approved = await call("POST", "/link/approve", {
      body: { user_code: started.body.user_code.replace("-", "").toLowerCase() },
      token: rae.token,
    });
    expect(approved.status).toBe(200);

    const linked = await call("POST", "/link/poll", {
      body: { device_code: started.body.device_code },
    });
    expect(linked.status).toBe(200);
    expect(linked.body.token).toBeTruthy();

    const who = await call("GET", "/me", { token: linked.body.token });
    expect(who.body.account.email).toBe("rae@example.com");
  });

  it("spends the link request, so the same code cannot be redeemed twice", async () => {
    const rae = await account("rae@example.com");
    const started = await call("POST", "/link/start", { body: { device: "TV" } });
    await call("POST", "/link/approve", {
      body: { user_code: started.body.user_code },
      token: rae.token,
    });

    const first = await call("POST", "/link/poll", {
      body: { device_code: started.body.device_code },
    });
    const second = await call("POST", "/link/poll", {
      body: { device_code: started.body.device_code },
    });
    expect(first.status).toBe(200);
    expect(second.status).toBe(404);
  });

  it("will not approve a code nobody is waiting on", async () => {
    const rae = await account("rae@example.com");
    const approved = await call("POST", "/link/approve", {
      body: { user_code: "ABCD-EFGH" },
      token: rae.token,
    });
    expect(approved.status).toBe(404);
  });

  it("refuses to approve anything without a signed-in account", async () => {
    const started = await call("POST", "/link/start", { body: { device: "TV" } });
    const approved = await call("POST", "/link/approve", {
      body: { user_code: started.body.user_code },
    });
    expect(approved.status).toBe(401);
  });

  it("does not let the code on the screen stand in for the device's secret", async () => {
    const rae = await account("rae@example.com");
    const started = await call("POST", "/link/start", { body: { device: "TV" } });
    await call("POST", "/link/approve", {
      body: { user_code: started.body.user_code },
      token: rae.token,
    });
    // Somebody who read the television across the room knows only this.
    const guessed = await call("POST", "/link/poll", {
      body: { device_code: started.body.user_code },
    });
    expect(guessed.status).toBe(404);
  });
});

describe("servers", () => {
  it("claims a server, which its owner is a member of", async () => {
    const rae = await account("rae@example.com");
    const house = await server(rae.token);

    const mine = await call("GET", "/me/servers", { token: rae.token });
    expect(mine.body.servers).toHaveLength(1);
    expect(mine.body.servers[0]).toMatchObject({ id: house.id, owned: true, addresses: [] });
  });

  it("takes addresses from the server itself and gives them to its members", async () => {
    const rae = await account("rae@example.com");
    const house = await server(rae.token);

    const set = await call("PUT", `/servers/${house.id}/addresses`, {
      body: { addresses: ["https://bms.example", "http://100.64.0.1:32400", "https://bms.example"] },
      token: house.secret,
    });
    expect(set.status).toBe(200);
    // Deduplicated, order kept.
    expect(set.body.addresses).toEqual(["https://bms.example", "http://100.64.0.1:32400"]);

    const mine = await call("GET", "/me/servers", { token: rae.token });
    expect(mine.body.servers[0].addresses).toEqual([
      "https://bms.example",
      "http://100.64.0.1:32400",
    ]);
  });

  it("will not take addresses from an account token, only the server's secret", async () => {
    const rae = await account("rae@example.com");
    const house = await server(rae.token);
    const set = await call("PUT", `/servers/${house.id}/addresses`, {
      body: { addresses: ["https://bms.example"] },
      token: rae.token,
    });
    expect(set.status).toBe(401);
  });
});

describe("invitations", () => {
  it("invites somebody, who accepts and can then see the server", async () => {
    const rae = await account("rae@example.com");
    const sam = await account("sam@example.com");
    const house = await server(rae.token);

    const invited = await call("POST", `/servers/${house.id}/invitations`, {
      body: { email: "sam@example.com" },
      token: rae.token,
    });
    expect(invited.status).toBe(201);

    const before = await call("GET", "/me/servers", { token: sam.token });
    expect(before.body.servers).toHaveLength(0);

    const accepted = await call("POST", `/invitations/${invited.body.invitation.code}/accept`, {
      token: sam.token,
    });
    expect(accepted.status).toBe(201);

    const after = await call("GET", "/me/servers", { token: sam.token });
    expect(after.body.servers).toHaveLength(1);
    expect(after.body.servers[0].owned).toBe(false);
  });

  it("refuses an invitation forwarded to somebody else", async () => {
    const rae = await account("rae@example.com");
    await account("sam@example.com");
    const mal = await account("mal@example.com");
    const house = await server(rae.token);

    const invited = await call("POST", `/servers/${house.id}/invitations`, {
      body: { email: "sam@example.com" },
      token: rae.token,
    });
    const stolen = await call("POST", `/invitations/${invited.body.invitation.code}/accept`, {
      token: mal.token,
    });
    expect(stolen.status).toBe(403);
  });

  it("spends an invitation on acceptance", async () => {
    const rae = await account("rae@example.com");
    const sam = await account("sam@example.com");
    const house = await server(rae.token);
    const invited = await call("POST", `/servers/${house.id}/invitations`, {
      body: { email: "sam@example.com" },
      token: rae.token,
    });
    const code = invited.body.invitation.code;

    expect((await call("POST", `/invitations/${code}/accept`, { token: sam.token })).status).toBe(
      201,
    );
    expect((await call("POST", `/invitations/${code}/accept`, { token: sam.token })).status).toBe(
      409,
    );
  });

  it("only lets the owner invite", async () => {
    const rae = await account("rae@example.com");
    const sam = await account("sam@example.com");
    const house = await server(rae.token);

    const invited = await call("POST", `/servers/${house.id}/invitations`, {
      body: { email: "mal@example.com" },
      token: sam.token,
    });
    expect(invited.status).toBe(404);
  });

  it("lists members to the server and removes one at the owner's word", async () => {
    const rae = await account("rae@example.com");
    const sam = await account("sam@example.com");
    const house = await server(rae.token);
    const invited = await call("POST", `/servers/${house.id}/invitations`, {
      body: { email: "sam@example.com" },
      token: rae.token,
    });
    await call("POST", `/invitations/${invited.body.invitation.code}/accept`, { token: sam.token });

    const listed = await call("GET", `/servers/${house.id}/members`, { token: house.secret });
    expect(listed.body.members).toHaveLength(2);
    expect(listed.body.members.find((m) => m.email === "rae@example.com").owner).toBe(true);

    const removed = await call("DELETE", `/servers/${house.id}/members/${sam.id}`, {
      token: rae.token,
    });
    expect(removed.status).toBe(204);

    const after = await call("GET", "/me/servers", { token: sam.token });
    expect(after.body.servers).toHaveLength(0);
  });

  it("will not remove the owner from their own server", async () => {
    const rae = await account("rae@example.com");
    const house = await server(rae.token);
    const removed = await call("DELETE", `/servers/${house.id}/members/${rae.id}`, {
      token: rae.token,
    });
    expect(removed.status).toBe(409);
  });
});

describe("assertions", () => {
  it("signs one a server can verify, addressed to that server alone", async () => {
    const rae = await account("rae@example.com");
    const house = await server(rae.token);

    const issued = await call("POST", `/servers/${house.id}/assertion`, { token: rae.token });
    expect(issued.status).toBe(200);

    const [header, payload, signature] = issued.body.assertion.split(".");
    const decode = (part) =>
      JSON.parse(new TextDecoder().decode(bytes(part)));
    expect(decode(header)).toMatchObject({ alg: "EdDSA", typ: "bask+jws", kid: "k1" });
    const claims = decode(payload);
    expect(claims).toMatchObject({
      iss: "https://account.example",
      sub: rae.id,
      aud: house.id,
      email: "rae@example.com",
    });
    expect(claims.exp - claims.iat).toBe(300);

    // And the signature is real, against the key the service publishes.
    const published = await call("GET", "/.well-known/bask-keys.json");
    const key = published.body.keys.find((k) => k.kid === "k1");
    const verifier = await crypto.subtle.importKey(
      "jwk",
      { kty: "OKP", crv: "Ed25519", x: key.key },
      { name: "Ed25519" },
      false,
      ["verify"],
    );
    const ok = await crypto.subtle.verify(
      { name: "Ed25519" },
      verifier,
      bytes(signature),
      new TextEncoder().encode(header + "." + payload),
    );
    expect(ok).toBe(true);
  });

  it("refuses one for a server the account is not a member of", async () => {
    const rae = await account("rae@example.com");
    const sam = await account("sam@example.com");
    const house = await server(rae.token);

    const issued = await call("POST", `/servers/${house.id}/assertion`, { token: sam.token });
    expect(issued.status).toBe(404);
  });

  it("stops issuing the moment a membership is revoked", async () => {
    const rae = await account("rae@example.com");
    const sam = await account("sam@example.com");
    const house = await server(rae.token);
    const invited = await call("POST", `/servers/${house.id}/invitations`, {
      body: { email: "sam@example.com" },
      token: rae.token,
    });
    await call("POST", `/invitations/${invited.body.invitation.code}/accept`, { token: sam.token });

    expect((await call("POST", `/servers/${house.id}/assertion`, { token: sam.token })).status).toBe(
      200,
    );
    await call("DELETE", `/servers/${house.id}/members/${sam.id}`, { token: rae.token });
    expect((await call("POST", `/servers/${house.id}/assertion`, { token: sam.token })).status).toBe(
      404,
    );
  });

  it("gives a stranger nothing, not even whether a server exists", async () => {
    const rae = await account("rae@example.com");
    const house = await server(rae.token);
    const mal = await account("mal@example.com");

    const real = await call("POST", `/servers/${house.id}/assertion`, { token: mal.token });
    const imaginary = await call("POST", "/servers/notaserveratall/assertion", { token: mal.token });
    expect(real.status).toBe(404);
    expect(imaginary.status).toBe(404);
    expect(real.body.error).toBe(imaginary.body.error);
  });
});

describe("the service itself", () => {
  it("publishes its public key and nothing resembling a private one", async () => {
    const published = await call("GET", "/.well-known/bask-keys.json");
    expect(published.status).toBe(200);
    expect(published.body.keys[0]).toMatchObject({ kid: "k1", alg: "EdDSA" });
    expect(JSON.stringify(published.body)).not.toContain(signingKey);
  });

  it("answers an unknown path with a 404 rather than a stack trace", async () => {
    const missing = await call("GET", "/nothing/here");
    expect(missing.status).toBe(404);
    expect(missing.body.error).toBeTruthy();
  });

  it("refuses a body that is not JSON", async () => {
    const request = new Request("https://account.example/sessions", {
      method: "POST",
      headers: { "content-type": "application/json", "cf-connecting-ip": "198.51.100.7" },
      body: "{not json",
    });
    const ctx = createExecutionContext();
    const response = await worker.fetch(request, configured(), ctx);
    await waitOnExecutionContext(ctx);
    expect(response.status).toBe(400);
  });

  it("throttles one address hammering it, and says how long to wait", async () => {
    // Against /link/start rather than sign-in: both are limited per address,
    // and this one does not spend 600,000 PBKDF2 iterations per attempt.
    let throttled = null;
    for (let attempt = 0; attempt < 80 && !throttled; attempt++) {
      const tried = await call("POST", "/link/start", {
        body: { device: "TV" },
        from: "198.51.100.42",
      });
      if (tried.status === 429) throttled = tried;
    }
    expect(throttled).not.toBeNull();
    expect(throttled.headers.get("retry-after")).toBeTruthy();
  });
});

function bytes(part) {
  const padded = part.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

describe("claiming a server", () => {
  it("lets a server claim itself with a token its owner minted", async () => {
    const rae = await account("rae@example.com");

    const minted = await call("POST", "/claim", { token: rae.token });
    expect(minted.status).toBe(201);
    expect(minted.body.token).toMatch(/^claim-/);
    expect(minted.body.expiresIn).toBe(240);

    // The server's own call. It has no account token and no secret yet: the
    // claim token is the whole of its authority, which is the point.
    const claimed = await call("POST", "/servers/claim", {
      body: { token: minted.body.token, name: "The house" },
    });
    expect(claimed.status).toBe(201);
    expect(claimed.body.server.name).toBe("The house");
    expect(claimed.body.secret).toBeTruthy();

    // And the owner can now see it, which is the whole purpose.
    const mine = await call("GET", "/me/servers", { token: rae.token });
    expect(mine.status).toBe(200);
    expect(mine.body.servers.map((s) => s.id)).toContain(claimed.body.server.id);
  });

  it("spends a claim token on the first server to use it", async () => {
    const rae = await account("spend@example.com");
    const minted = await call("POST", "/claim", { token: rae.token });

    const first = await call("POST", "/servers/claim", { body: { token: minted.body.token } });
    expect(first.status).toBe(201);

    const second = await call("POST", "/servers/claim", { body: { token: minted.body.token } });
    expect(second.status).toBe(404);

    // One server, not two, however many times the token was presented.
    const mine = await call("GET", "/me/servers", { token: rae.token });
    expect(mine.body.servers).toHaveLength(1);
  });

  it("refuses a claim token that has expired", async () => {
    const rae = await account("stale@example.com");
    const minted = await call("POST", "/claim", { token: rae.token });

    // Reach into the table rather than wait four minutes.
    await env.DB.prepare(`UPDATE claim_tokens SET expires_at = 1`).run();

    const late = await call("POST", "/servers/claim", { body: { token: minted.body.token } });
    expect(late.status).toBe(404);
  });

  it("says the same thing about a token that never existed", async () => {
    const spent = await account("same@example.com");
    const minted = await call("POST", "/claim", { token: spent.token });
    await call("POST", "/servers/claim", { body: { token: minted.body.token } });

    const used = await call("POST", "/servers/claim", { body: { token: minted.body.token } });
    const never = await call("POST", "/servers/claim", { body: { token: "claim-nonsense" } });

    // A different answer for "spent" and "never existed" would tell somebody
    // guessing whether a guess had ever been a real token.
    expect(never.status).toBe(used.status);
    expect(never.body.error).toBe(used.body.error);
  });

  it("will not mint a claim token for somebody who is not signed in", async () => {
    const anonymous = await call("POST", "/claim");
    expect(anonymous.status).toBe(401);
  });
});

/* ---------- the pages a person uses ---------- */

describe("the pages", () => {
  // A browser's request, as opposed to a program's: a form post, an Origin,
  // and a cookie instead of an Authorization header.
  async function visit(method, path, { fields, cookie, origin, site } = {}) {
    const headers = { "cf-connecting-ip": `198.51.100.${++caller % 250}` };
    if (cookie) headers.cookie = cookie;
    if (site !== null) headers["sec-fetch-site"] = site || "same-origin";
    if (origin) headers.origin = origin;

    let body;
    if (fields) {
      body = new URLSearchParams(fields);
      headers["content-type"] = "application/x-www-form-urlencoded";
    }

    const request = new Request("https://account.example" + path, { method, headers, body });
    const ctx = createExecutionContext();
    const response = await worker.fetch(request, configured(), ctx);
    await waitOnExecutionContext(ctx);
    return {
      status: response.status,
      location: response.headers.get("location"),
      cookie: response.headers.get("set-cookie"),
      type: response.headers.get("content-type"),
      csp: response.headers.get("content-security-policy"),
      text: await response.text(),
    };
  }

  /** signedIn registers through the sign-up form and keeps the cookie. */
  async function signedIn(email, password = "a-long-enough-password") {
    const made = await visit("POST", "/web/sign-up", {
      fields: { email, password, name: "Somebody" },
    });
    expect(made.status).toBe(303);
    expect(made.cookie).toBeTruthy();
    return made.cookie.split(";")[0];
  }

  it("offers a sign-in form to a stranger", async () => {
    const home = await visit("GET", "/");
    expect(home.status).toBe(200);
    expect(home.type).toContain("text/html");
    expect(home.text).toContain('action="/web/sign-in"');
  });

  it("forbids script on every page it serves", async () => {
    const home = await visit("GET", "/");
    // The pages carry no script at all, so this is not a mitigation that has
    // to be kept in step with a bundle: it is the actual shape of the page.
    expect(home.csp).toContain("default-src 'none'");
    expect(home.csp).not.toContain("script-src");
    expect(home.text).not.toContain("<script");
  });

  it("creates an account from the form and lands on the servers page", async () => {
    const cookie = await signedIn("formed@example.com");
    const servers = await visit("GET", "/servers", { cookie });
    expect(servers.status).toBe(200);
    expect(servers.text).toContain("No servers yet");
  });

  it("keeps the session cookie out of reach of script and of other sites", async () => {
    const made = await visit("POST", "/web/sign-up", {
      fields: { email: "cookie@example.com", password: "a-long-enough-password" },
    });
    expect(made.cookie).toContain("HttpOnly");
    expect(made.cookie).toContain("Secure");
    expect(made.cookie).toContain("SameSite=Strict");
  });

  it("signs in an existing account", async () => {
    await signedIn("returning@example.com");
    const again = await visit("POST", "/web/sign-in", {
      fields: { email: "returning@example.com", password: "a-long-enough-password" },
    });
    expect(again.status).toBe(303);
    expect(again.location).toBe("/servers");
  });

  it("says so, on the form, when the password is wrong", async () => {
    await signedIn("wrong@example.com");
    const refused = await visit("POST", "/web/sign-in", {
      fields: { email: "wrong@example.com", password: "not-the-password" },
    });
    expect(refused.status).toBe(401);
    expect(refused.text).toContain("do not match");
    // Re-rendered with the address kept, because retyping it is a small
    // insult on top of a failed sign-in.
    expect(refused.text).toContain("wrong@example.com");
    expect(refused.cookie).toBeNull();
  });

  it("refuses a form that did not come from here", async () => {
    await signedIn("csrf@example.com");
    const crossSite = await visit("POST", "/web/sign-in", {
      fields: { email: "csrf@example.com", password: "a-long-enough-password" },
      site: "cross-site",
    });
    expect(crossSite.status).toBe(403);
    expect(crossSite.cookie).toBeNull();
  });

  it("refuses a form from a client that claims no origin at all", async () => {
    const nothing = await visit("POST", "/web/sign-in", {
      fields: { email: "nobody@example.com", password: "a-long-enough-password" },
      site: null,
    });
    expect(nothing.status).toBe(403);
  });

  it("will not let the cookie drive an API route", async () => {
    const cookie = await signedIn("api@example.com");
    // accountFor reads the Authorization header and nothing else, so a cookie
    // that a page could be tricked into sending cannot mint anything.
    const minted = await call("POST", "/claim");
    expect(minted.status).toBe(401);

    const request = new Request("https://account.example/claim", {
      method: "POST",
      headers: { cookie, "cf-connecting-ip": "198.51.100.251" },
    });
    const ctx = createExecutionContext();
    const response = await worker.fetch(request, configured(), ctx);
    await waitOnExecutionContext(ctx);
    expect(response.status).toBe(401);
  });

  it("mints a claim token on the page and shows it once", async () => {
    const cookie = await signedIn("claimer@example.com");
    const minted = await visit("POST", "/web/claim", { cookie, fields: {} });
    expect(minted.status).toBe(200);
    expect(minted.text).toMatch(/claim-[A-Za-z0-9_-]+/);

    // And it is a real one: the server half redeems it.
    const token = minted.text.match(/claim-[A-Za-z0-9_-]+/)[0];
    const claimed = await call("POST", "/servers/claim", { body: { token } });
    expect(claimed.status).toBe(201);
  });

  it("lists a claimed server with the addresses it publishes", async () => {
    const cookie = await signedIn("owner@example.com");
    const minted = await visit("POST", "/web/claim", { cookie, fields: {} });
    const token = minted.text.match(/claim-[A-Za-z0-9_-]+/)[0];
    const claimed = await call("POST", "/servers/claim", { body: { token, name: "The attic" } });

    await call("PUT", `/servers/${claimed.body.server.id}/addresses`, {
      body: { addresses: ["https://attic.example:32500"] },
      token: claimed.body.secret,
    });

    const servers = await visit("GET", "/servers", { cookie });
    expect(servers.text).toContain("The attic");
    expect(servers.text).toContain("attic.example:32500");
  });

  it("approves a waiting device from the page", async () => {
    const cookie = await signedIn("linker@example.com");
    const started = await call("POST", "/link/start", {
      body: { client: "Bask", device: "The big television" },
    });
    expect(started.status).toBe(200);

    const approved = await visit("POST", "/web/link", {
      cookie,
      fields: { user_code: started.body.user_code },
    });
    expect(approved.status).toBe(200);
    expect(approved.text).toContain("The big television");

    const polled = await call("POST", "/link/poll", {
      body: { device_code: started.body.device_code },
    });
    expect(polled.body.status).toBe("linked");
  });

  it("says so on the page when a code is not waiting", async () => {
    const cookie = await signedIn("badcode@example.com");
    const refused = await visit("POST", "/web/link", {
      cookie,
      fields: { user_code: "ZZZZ-ZZZZ" },
    });
    expect(refused.status).toBe(400);
    expect(refused.text).toContain("not waiting for approval");
  });

  it("sends a stranger away from the servers page without a redirect loop", async () => {
    const servers = await visit("GET", "/servers");
    expect(servers.status).toBe(303);
    expect(servers.location).toBe("/");
  });

  it("sends somebody already signed in past the sign-in form", async () => {
    const cookie = await signedIn("already@example.com");
    const home = await visit("GET", "/", { cookie });
    expect(home.status).toBe(303);
    expect(home.location).toBe("/servers");
  });

  it("clears the cookie on sign-out", async () => {
    const cookie = await signedIn("leaving@example.com");
    const out = await visit("POST", "/web/sign-out", { cookie, fields: {} });
    expect(out.status).toBe(303);
    expect(out.cookie).toContain("Max-Age=0");
  });

  it("will not create an account the form would then reject", async () => {
    const short = await visit("POST", "/web/sign-up", {
      fields: { email: "short@example.com", password: "tiny" },
    });
    expect(short.status).toBe(400);
    expect(short.text).toContain("ten characters");
    expect(short.cookie).toBeNull();

    const taken = await signedIn("taken@example.com");
    expect(taken).toBeTruthy();
    const again = await visit("POST", "/web/sign-up", {
      fields: { email: "taken@example.com", password: "a-long-enough-password" },
    });
    expect(again.status).toBe(409);
    expect(again.text).toContain("already an account");
  });

  it("escapes what somebody typed rather than rendering it", async () => {
    // A server name comes from whoever claimed it, and it is shown on a page.
    const cookie = await signedIn("escaping@example.com");
    const minted = await visit("POST", "/web/claim", { cookie, fields: {} });
    const token = minted.text.match(/claim-[A-Za-z0-9_-]+/)[0];
    await call("POST", "/servers/claim", {
      body: { token, name: '<img src=x onerror="alert(1)">' },
    });

    const servers = await visit("GET", "/servers", { cookie });
    expect(servers.text).not.toContain("<img src=x");
    expect(servers.text).toContain("&lt;img src=x");
  });

  it("serves the page a device sends somebody to", async () => {
    // linkStart hands a device verification_uri = PUBLIC_ORIGIN + "/link",
    // so that path has to render rather than 404.
    const cookie = await signedIn("visiting@example.com");
    const started = await call("POST", "/link/start", {
      body: { client: "Bask", device: "The set-top box" },
    });

    const prompt = await visit("GET", `/link?code=${started.body.user_code}`, { cookie });
    expect(prompt.status).toBe(200);
    expect(prompt.text).toContain(started.body.user_code);
  });

  it("sends a stranger at /link to sign in, then back to /link", async () => {
    const prompt = await visit("GET", "/link");
    expect(prompt.status).toBe(200);
    expect(prompt.text).toContain('action="/web/sign-in"');
    expect(prompt.text).toContain('value="/link"');

    await signedIn("afterlink@example.com");
    const back = await visit("POST", "/web/sign-in", {
      fields: {
        email: "afterlink@example.com",
        password: "a-long-enough-password",
        next: "/link",
      },
    });
    expect(back.location).toBe("/link");
  });

  it("will not be redirected somewhere else after signing in", async () => {
    await signedIn("offsite@example.com");
    for (const next of ["https://elsewhere.example/", "//elsewhere.example/", "/\\evil"]) {
      const away = await visit("POST", "/web/sign-in", {
        fields: { email: "offsite@example.com", password: "a-long-enough-password", next },
      });
      expect(away.location).toBe("/servers");
    }
  });

  it("answers HEAD the way it answers GET", async () => {
    // A monitor, a link checker and a browser's preflight all send HEAD, and
    // routing it as anything but GET lands them at the 404 at the bottom of
    // the table.
    const head = await visit("HEAD", "/");
    expect(head.status).toBe(200);
    expect(head.type).toContain("text/html");
  });

  it("serves its stylesheet as CSS", async () => {
    const css = await visit("GET", "/styles.css");
    expect(css.status).toBe(200);
    expect(css.type).toContain("text/css");
  });
});
