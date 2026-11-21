// The small amount of HTTP this service does the same way everywhere.

/** json is every successful answer. */
export function json(body, status = 200, headers = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      // An account service's answers are about right now and belong in no
      // cache, anybody's.
      "cache-control": "no-store",
      ...headers,
    },
  });
}

/**
 * fail is every unsuccessful one.
 *
 * One shape, and a message written for the person who will read it in a
 * client's error toast rather than for the developer reading a log.
 */
export function fail(status, message, headers = {}) {
  return json({ error: message }, status, headers);
}

/**
 * readJSON reads a small body, or answers the client itself.
 *
 * Capped because nothing here legitimately sends more than a few hundred
 * bytes, and an uncapped read is somebody else deciding how much memory this
 * request uses.
 */
export async function readJSON(request, limit = 4096) {
  const text = await request.text();
  if (text.length > limit) return { error: fail(413, "that request is too large") };
  if (!text) return { value: {} };
  try {
    return { value: JSON.parse(text) };
  } catch {
    return { error: fail(400, "that request is not JSON") };
  }
}

/** bearer reads the Authorization header's token, or "". */
export function bearer(request) {
  const header = request.headers.get("authorization") || "";
  const [scheme, value] = header.split(" ");
  if (!scheme || scheme.toLowerCase() !== "bearer" || !value) return "";
  return value.trim();
}

/** now is seconds since the epoch, which is how every time here is stored. */
export function now() {
  return Math.floor(Date.now() / 1000);
}

/**
 * clientAddress is who to rate limit, from the header Cloudflare sets.
 *
 * Never read from an X-Forwarded-For a client wrote: that is a field anybody
 * can fill in, and rate limiting on it limits whoever the attacker names.
 */
export function clientAddress(request) {
  return request.headers.get("cf-connecting-ip") || "";
}

/**
 * limit asks the rate limiter whether this may proceed.
 *
 * The binding is optional so the service runs in a test and in `wrangler dev`
 * without one. That is a deliberate hole in exactly one direction: an
 * unconfigured deployment is slower to abuse than an unprotected one only
 * because nobody knows about it, so the deploy configuration binds it and the
 * README says what happens if it does not.
 */
export async function limit(env, key) {
  if (!env.RATE_LIMIT || typeof env.RATE_LIMIT.limit !== "function") return true;
  const { success } = await env.RATE_LIMIT.limit({ key });
  return success;
}

/** tooMany is the one answer a limiter gives, with the wait a client honours. */
export function tooMany(seconds = 60) {
  return fail(429, "too many attempts; wait a moment", { "retry-after": String(seconds) });
}

/**
 * email folds an address for lookup.
 *
 * Case only. Nothing clever about plus addressing or dots, because stripping
 * those decides that two addresses the mail system treats as different are the
 * same person, and being wrong about that locks somebody out of their own
 * account.
 */
export function foldEmail(address) {
  return String(address || "")
    .trim()
    .toLowerCase();
}

/** looksLikeEmail is the weakest check that is still worth making. */
export function looksLikeEmail(address) {
  const value = String(address || "").trim();
  if (value.length < 3 || value.length > 254) return false;
  const at = value.indexOf("@");
  return at > 0 && at === value.lastIndexOf("@") && at < value.length - 1 && !/\s/.test(value);
}
