// The pages a person uses, as opposed to the API a program uses.
//
// Everything here is server-rendered HTML and plain forms. There is no script
// on any of these pages, which is why the content security policy can forbid
// script outright: an account service is the last place that should be asking
// anybody to trust a bundle.
//
// Browser requests authenticate with a cookie rather than the Authorization
// header the API uses, and the two are deliberately separate. accountFor stays
// bearer-only, so no amount of cookie in a browser can drive an API route from
// another site's page; these routes read the cookie, and they are the only
// ones that do.

import { hash, random } from "./crypto.js";
import { clientAddress, fail, foldEmail, limit, looksLikeEmail, now, tooMany } from "./http.js";

const COOKIE = "bask_session";
// A month. Long enough not to be a nuisance, short enough that an abandoned
// browser stops being a way in.
const SESSION_LIFETIME = 30 * 24 * 60 * 60;

/* ---------- plumbing ---------- */

/** escape makes a value safe to put in HTML text or a quoted attribute. */
export function escape(value) {
  return String(value == null ? "" : value)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function page(body, status = 200, headers = {}) {
  return new Response(body, {
    status,
    headers: {
      "content-type": "text/html; charset=utf-8",
      // No script anywhere, so nothing injected into a page can execute even
      // if an escaping mistake ever lets it through.
      "content-security-policy":
        "default-src 'none'; style-src 'self'; img-src 'self' data:; " +
        "form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
      "referrer-policy": "no-referrer",
      "x-content-type-options": "nosniff",
      "cache-control": "no-store",
      ...headers,
    },
  });
}

function redirect(to, headers = {}) {
  return new Response(null, { status: 303, headers: { location: to, "cache-control": "no-store", ...headers } });
}

function sessionCookie(token) {
  return `${COOKIE}=${token}; Path=/; Max-Age=${SESSION_LIFETIME}; HttpOnly; Secure; SameSite=Strict`;
}

function clearedCookie() {
  return `${COOKIE}=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict`;
}

function cookieToken(request) {
  const header = request.headers.get("cookie") || "";
  for (const part of header.split(";")) {
    const [name, ...rest] = part.trim().split("=");
    if (name === COOKIE) return rest.join("=").trim();
  }
  return "";
}

/**
 * sessionAccount resolves the session cookie, or null.
 *
 * The cookie holds an ordinary client token, so a browser session is a device
 * in the same list as a television, and revoking it is the same operation.
 */
export async function sessionAccount(request, env) {
  const token = cookieToken(request);
  if (!token) return null;
  const row = await env.DB.prepare(
    `SELECT a.id, a.email, a.name
       FROM clients c JOIN accounts a ON a.id = c.account_id
      WHERE c.token_hash = ?`,
  )
    .bind(await hash(token))
    .first();
  return row ? { id: row.id, email: row.email, name: row.name } : null;
}

/**
 * sameOrigin rejects a form submitted from somewhere else.
 *
 * SameSite=Strict already means the cookie is not sent with a cross-site
 * request, so this is the second lock rather than the first. It checks Origin
 * where the browser sends one and Sec-Fetch-Site where it does, and refuses a
 * request that contradicts either — a browser old enough to send neither
 * cannot be distinguished from a script, so it is also refused.
 */
function sameOrigin(request, url) {
  const site = request.headers.get("sec-fetch-site");
  if (site) return site === "same-origin";
  const origin = request.headers.get("origin");
  return Boolean(origin) && origin === url.origin;
}

async function form(request) {
  const body = await request.formData().catch(() => null);
  if (!body) return null;
  const out = {};
  for (const [key, value] of body.entries()) out[key] = typeof value === "string" ? value : "";
  return out;
}

/* ---------- chrome ---------- */

function shell({ title, account, body, lead = "" }) {
  const nav = account
    ? `<nav aria-label="Account">
        <span class="who">${escape(account.email)}</span>
        <form method="post" action="/web/sign-out"><button class="plain" type="submit">Sign out</button></form>
      </nav>`
    : "";
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${escape(title)} — Bask account</title>
<meta name="color-scheme" content="dark">
<meta name="robots" content="noindex">
<link rel="stylesheet" href="/styles.css">
</head>
<body>
<header class="masthead">
  <div class="wrap">
    <a class="wordmark" href="/">Bask <span>account</span></a>
    ${nav}
  </div>
</header>
<main class="wrap">
  <h1>${escape(title)}</h1>
  ${lead ? `<p class="lead">${lead}</p>` : ""}
  ${body}
</main>
<footer>
  <div class="wrap">
    <p>This account holds your sign-in, your devices and your servers' addresses. It does not hold your library.</p>
    <nav aria-label="Elsewhere">
      <a href="https://baskmedia.app/account/">What this is for</a>
      <a href="https://baskmedia.app/">Bask</a>
    </nav>
  </div>
</footer>
</body>
</html>
`;
}

function problem(message) {
  return message ? `<p class="problem" role="alert">${escape(message)}</p>` : "";
}

/* ---------- signing in ---------- */

function signInPage({ message = "", email = "", next = "" } = {}) {
  const hidden = next ? `<input type="hidden" name="next" value="${escape(next)}">` : "";
  return shell({
    title: "Sign in",
    lead:
      "An account is only needed so a player away from home can find your server, " +
      'and so you can share it with family. <a href="https://baskmedia.app/account/">What it holds</a>.',
    body: `
    ${problem(message)}
    <form class="card" method="post" action="/web/sign-in">
      ${hidden}
      <label for="email">Email address</label>
      <input id="email" name="email" type="email" autocomplete="username" required value="${escape(email)}">

      <label for="password">Password</label>
      <input id="password" name="password" type="password" autocomplete="current-password" required>

      <button type="submit">Sign in</button>
    </form>
    <p class="aside">No account yet? <a href="/sign-up">Create one</a>.</p>`,
  });
}

function signUpPage({ message = "", email = "", name = "" } = {}) {
  return shell({
    title: "Create an account",
    lead:
      "Free, and nothing about it is required to use either program: a player can always " +
      "be pointed straight at a server by address.",
    body: `
    ${problem(message)}
    <form class="card" method="post" action="/web/sign-up">
      <label for="name">Your name <span class="hint">optional, and only used to label an invitation</span></label>
      <input id="name" name="name" type="text" autocomplete="name" value="${escape(name)}">

      <label for="email">Email address</label>
      <input id="email" name="email" type="email" autocomplete="username" required value="${escape(email)}">

      <label for="password">Password <span class="hint">at least ten characters</span></label>
      <input id="password" name="password" type="password" autocomplete="new-password" minlength="10" required>

      <button type="submit">Create the account</button>
    </form>
    <p class="aside">Already have one? <a href="/">Sign in</a>.</p>`,
  });
}

function linkPage(account, code, { message = "" } = {}) {
  return shell({
    title: "Link a device",
    account,
    lead:
      "A player with no keyboard shows a short code. Type it here and that device is signed in " +
      "as you.",
    body: `
    ${problem(message)}
    <form class="card" method="post" action="/web/link">
      <label for="code">Code</label>
      <input id="code" name="user_code" type="text" inputmode="latin" autocomplete="off"
             spellcheck="false" placeholder="XXXX-XXXX" required value="${escape(code)}">
      <button type="submit">Approve</button>
    </form>
    <p class="aside"><a href="/servers">Your servers</a></p>`,
  });
}

/* ---------- the servers page ---------- */

function serversPage(account, servers, { claim = null, message = "", notice = "" } = {}) {
  const rows =
    servers.length === 0
      ? `<p class="empty">
           No servers yet. Install Bask Media Server, open its administration interface, and
           claim it with a token from below.
           <a href="https://baskmedia.app/install/">How to install it</a>.
         </p>`
      : servers
          .map(
            (server) => `
      <div class="server">
        <h3>${escape(server.name)}</h3>
        <div class="meta"><span>${server.owned ? "yours" : "shared with you"}</span><span>${escape(server.id)}</span></div>
        ${
          server.addresses.length
            ? `<ul class="addresses">${server.addresses
                .map((address) => `<li><code>${escape(address)}</code></li>`)
                .join("")}</ul>`
            : `<p class="empty">It has not published an address yet. A server does that itself, once it is running.</p>`
        }
      </div>`,
          )
          .join("");

  const minted = claim
    ? `<div class="token">
         <p>Paste this into the server's administration interface:</p>
         <p><code class="big">${escape(claim.token)}</code></p>
         <p class="hint">
           Good for ${Math.round(claim.expiresIn / 60)} minutes, and good once. The server spends it
           itself, so your password never goes near it.
         </p>
       </div>`
    : `<form method="post" action="/web/claim">
         <button type="submit">Mint a claim token</button>
       </form>`;

  return shell({
    title: "Your servers",
    account,
    body: `
    ${problem(message)}
    ${notice ? `<p class="notice">${escape(notice)}</p>` : ""}
    <div class="servers">${rows}</div>

    <section>
      <h2>Claim a server</h2>
      <p>
        A token good for four minutes and for one use. You read it to the server and the server
        redeems it on its own behalf, so it never handles your password and you never carry a
        long-lived secret from one machine to another.
      </p>
      ${minted}
    </section>

    <section>
      <h2>Link a device</h2>
      <p>
        A television with no keyboard shows a short code. Type it here and that device is signed in
        as you.
      </p>
      <form class="inline" method="post" action="/web/link">
        <label for="code">Code</label>
        <input id="code" name="user_code" type="text" inputmode="latin" autocomplete="off"
               spellcheck="false" placeholder="XXXX-XXXX" required>
        <button type="submit">Approve</button>
      </form>
    </section>`,
  });
}

/* ---------- handlers ---------- */

export async function webRoute(request, env, url, path, method) {
  if (method === "GET" && path === "/styles.css") return styles();

  if (method === "GET" && path === "/") {
    const account = await sessionAccount(request, env);
    if (account) return redirect("/servers");
    return page(signInPage());
  }

  if (method === "GET" && path === "/sign-up") {
    const account = await sessionAccount(request, env);
    if (account) return redirect("/servers");
    return page(signUpPage());
  }

  if (method === "GET" && path === "/link") {
    const account = await sessionAccount(request, env);
    const code = (url.searchParams.get("code") || "").slice(0, 16);
    if (!account) return page(signInPage({ next: "/link" }));
    return page(linkPage(account, code));
  }

  if (method === "GET" && path === "/servers") {
    const account = await sessionAccount(request, env);
    if (!account) return redirect("/");
    return page(serversPage(account, await serversFor(env, account.id)));
  }

  if (method === "POST" && path.startsWith("/web/")) return webPost(request, env, url, path);

  // A GET of a form target, which is what a browser does when somebody goes
  // back to one. Send them somewhere that renders.
  if (method === "GET" && path.startsWith("/web/")) return redirect("/");

  return null;
}

async function webPost(request, env, url, path) {
  if (!sameOrigin(request, url)) return fail(403, "that form did not come from here");

  if (path === "/web/sign-out") {
    return redirect("/", { "set-cookie": clearedCookie() });
  }

  const fields = await form(request);
  if (!fields) return fail(400, "that form did not arrive in one piece");

  switch (path) {
    case "/web/sign-in":
      return signInSubmit(request, env, fields);
    case "/web/sign-up":
      return signUpSubmit(request, env, fields);
    case "/web/claim":
      return claimSubmit(request, env);
    case "/web/link":
      return linkSubmit(request, env, fields);
    default:
      return fail(404, "no such form");
  }
}

async function signInSubmit(request, env, fields) {
  if (!(await limit(env, "signin:" + clientAddress(request)))) return tooMany();

  const email = String(fields.email || "").trim();
  const password = String(fields.password || "");
  const account = await authenticate(env, email, password);
  if (!account) {
    if (!(await limit(env, "signin-fail:" + foldEmail(email)))) return tooMany();
    return page(
      signInPage({ message: "That email address and password do not match.", email }),
      401,
    );
  }

  const token = await issueBrowser(env, account.id);
  return redirect(destination(fields.next), { "set-cookie": sessionCookie(token) });
}

/**
 * destination is where to go after signing in.
 *
 * Only a path on this service. Anything that could be read as another origin
 * — a scheme, a protocol-relative "//host", a backslash some browsers treat
 * as a slash — is discarded rather than corrected, because an open redirect on
 * a sign-in form is how a convincing phishing link gets made.
 */
function destination(next) {
  const wanted = String(next || "");
  if (!wanted.startsWith("/") || wanted.startsWith("//") || wanted.includes("\\")) {
    return "/servers";
  }
  return wanted;
}

async function signUpSubmit(request, env, fields) {
  if (!(await limit(env, "register:" + clientAddress(request)))) return tooMany();

  const email = String(fields.email || "").trim();
  const password = String(fields.password || "");
  const name = String(fields.name || "").trim().slice(0, 64);

  const again = (message, status) => page(signUpPage({ message, email, name }), status);
  if (!looksLikeEmail(email)) return again("That does not look like an email address.", 400);
  if (password.length < 10) return again("A password needs at least ten characters.", 400);
  if (password.length > 512) return again("That password is unreasonably long.", 400);

  const created = await createAccount(env, { email, password, name });
  if (!created) return again("There is already an account with that address.", 409);

  const token = await issueBrowser(env, created.id);
  return redirect("/servers", { "set-cookie": sessionCookie(token) });
}

async function claimSubmit(request, env) {
  const account = await sessionAccount(request, env);
  if (!account) return redirect("/");
  if (!(await limit(env, "claim:" + account.id))) return tooMany();

  const claim = await mintClaimToken(env, account.id);
  return page(serversPage(account, await serversFor(env, account.id), { claim }));
}

async function linkSubmit(request, env, fields) {
  const account = await sessionAccount(request, env);
  if (!account) return redirect("/");
  if (!(await limit(env, "approve:" + account.id))) return tooMany();

  const approved = await approveCode(env, account.id, fields.user_code);
  const servers = await serversFor(env, account.id);
  if (!approved.ok) {
    return page(serversPage(account, servers, { message: approved.why }), 400);
  }
  return page(
    serversPage(account, servers, {
      notice: approved.device
        ? `${approved.device} is signed in.`
        : "That device is signed in.",
    }),
  );
}

/* ---------- the operations these pages and the API share ----------
   Injected by index.js rather than imported, because the SQL for each of them
   lives beside the API route that already had it. Two copies of the sign-in
   comparison is exactly the kind of thing that drifts. */

let authenticate, createAccount, issueBrowser, mintClaimToken, serversFor, approveCode;

export function useOperations(operations) {
  ({ authenticate, createAccount, issueBrowser, mintClaimToken, serversFor, approveCode } =
    operations);
}

/* ---------- styling ---------- */

// The same visual language as baskmedia.app, with no font to fetch and no
// third party involved in a page somebody types a password into.
function styles() {
  const css = `
:root {
  --ink:#1b2127; --well:#141a1f; --panel:#232a31; --panel-high:#2a323a;
  --rule:#343d46; --rule-soft:#2b333b; --text:#e7ecef; --text-dim:#9daab4;
  --text-faint:#6f7c86; --signal:#5fd0e0; --signal-dim:#2f6b76;
  --warn:#e4a1a1;
  --sans: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  --mono: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
}
*,*::before,*::after { box-sizing: border-box; }
body {
  margin:0; background:var(--ink); color:var(--text);
  font-family:var(--sans); font-size:1rem; line-height:1.6;
  -webkit-font-smoothing:antialiased;
}
.wrap { max-width:46rem; margin:0 auto; padding-inline:clamp(1.25rem,5vw,2.5rem); }
h1,h2,h3 { margin:0; font-weight:600; letter-spacing:-0.018em; line-height:1.15; }
a { color:var(--text); text-decoration:none; border-bottom:1px solid var(--signal-dim); }
a:hover { border-bottom-color:var(--signal); }
a:focus-visible, button:focus-visible, input:focus-visible {
  outline:2px solid var(--signal); outline-offset:2px;
}
code { font-family:var(--mono); font-size:0.875em; }

.masthead { border-bottom:1px solid var(--rule-soft); background:var(--well); }
.masthead .wrap {
  display:flex; align-items:center; justify-content:space-between;
  gap:1rem; padding-block:1rem; flex-wrap:wrap;
}
.wordmark { font-weight:600; border:0; display:inline-flex; gap:0.5rem; }
.wordmark span { color:var(--text-faint); font-weight:400; }
.masthead nav { display:flex; align-items:center; gap:1rem; font-size:0.9375rem; }
.who { color:var(--text-faint); font-family:var(--mono); font-size:0.8125rem; }
.masthead form { margin:0; }
button.plain {
  background:none; border:0; border-bottom:1px solid var(--signal-dim);
  color:var(--text-dim); font:inherit; font-size:0.9375rem; padding:0; cursor:pointer;
}
button.plain:hover { color:var(--text); border-bottom-color:var(--signal); }

main { padding-block:clamp(2.5rem,6vw,4rem) 3rem; }
main h1 { font-size:clamp(1.75rem,4vw,2.5rem); }
.lead { margin:1rem 0 0; color:var(--text-dim); max-width:58ch; }

.card {
  margin-top:clamp(1.75rem,4vw,2.5rem); padding:clamp(1.25rem,3vw,1.75rem);
  border:1px solid var(--rule); border-radius:3px; background:var(--panel);
  display:grid; gap:0.4rem;
}
label { font-size:0.875rem; font-weight:600; margin-top:0.6rem; }
label:first-child { margin-top:0; }
.hint { display:block; font-weight:400; color:var(--text-faint); font-size:0.8125rem; }
input {
  font:inherit; color:var(--text); background:var(--well);
  border:1px solid var(--rule); border-radius:2px; padding:0.6rem 0.7rem; min-width:0;
}
input:hover { border-color:var(--text-faint); }
button[type=submit] {
  margin-top:1.1rem; justify-self:start; font:inherit; font-weight:600;
  padding:0.6rem 1.15rem; border:1px solid var(--signal); border-radius:2px;
  background:var(--signal); color:var(--well); cursor:pointer;
}
button[type=submit]:hover { background:#7cdbe8; border-color:#7cdbe8; }
form.inline {
  margin-top:1.25rem; display:flex; flex-wrap:wrap; align-items:end; gap:0.75rem;
}
form.inline label { margin:0; }
form.inline button[type=submit] { margin:0; }

.aside { margin-top:1.5rem; color:var(--text-dim); font-size:0.9375rem; }
.problem {
  margin:1.5rem 0 0; padding:0.8rem 1rem; border:1px solid var(--warn);
  border-left-width:3px; border-radius:2px; color:var(--warn); font-size:0.9375rem;
}
.notice {
  margin:1.5rem 0 0; padding:0.8rem 1rem; border:1px solid var(--signal-dim);
  border-left-width:3px; border-radius:2px; color:var(--text-dim); font-size:0.9375rem;
}
.empty { color:var(--text-faint); font-size:0.9375rem; margin:0.5rem 0 0; }

.servers { margin-top:clamp(1.75rem,4vw,2.5rem); display:grid; gap:1px;
  background:var(--rule-soft); border:1px solid var(--rule-soft); border-radius:3px; }
.server { background:var(--panel); padding:clamp(1.1rem,3vw,1.5rem); }
.server h3 { font-size:1.0625rem; }
.server .meta { margin-top:0.4rem; display:flex; flex-wrap:wrap; gap:0.3rem 1rem;
  font-family:var(--mono); font-size:0.75rem; color:var(--text-faint); }
.addresses { margin:0.9rem 0 0; padding:0; list-style:none; display:grid; gap:0.3rem; }
.addresses code { color:var(--text-dim); }

section { margin-top:clamp(2.25rem,5vw,3rem); padding-top:clamp(1.75rem,4vw,2.25rem);
  border-top:1px solid var(--rule-soft); }
section h2 { font-size:1.1875rem; }
section p { margin:0.7rem 0 0; color:var(--text-dim); max-width:58ch; font-size:0.9875rem; }
section form { margin-top:1.25rem; }

.token { margin-top:1.25rem; padding:clamp(1.1rem,3vw,1.5rem);
  border:1px solid var(--rule); border-left:2px solid var(--signal);
  border-radius:3px; background:var(--panel); }
.token p { margin:0; color:var(--text-dim); font-size:0.9375rem; }
.token p + p { margin-top:0.8rem; }
.token .big { font-size:1.125rem; color:var(--text); word-break:break-all; }
.token .hint { display:block; color:var(--text-faint); font-size:0.875rem; }

footer { border-top:1px solid var(--rule-soft); background:var(--well); padding-block:2rem 2.5rem; }
footer .wrap { display:flex; flex-wrap:wrap; gap:0.75rem 2rem;
  justify-content:space-between; align-items:baseline; max-width:46rem; }
footer p { margin:0; color:var(--text-faint); font-size:0.8125rem; max-width:48ch; }
footer nav { display:flex; gap:1.25rem; font-size:0.8125rem; }
footer nav a { border:0; color:var(--text-dim); }
footer nav a:hover { color:var(--text); }

@media (prefers-color-scheme: light) { :root { --ink:#10161b; } }
`;
  return new Response(css.trim(), {
    headers: {
      "content-type": "text/css; charset=utf-8",
      "cache-control": "public, max-age=3600",
    },
  });
}
