// Everything secret in the service goes through here.
//
// The runtime gives us SubtleCrypto and nothing else, so the choices are made
// by what it has: PBKDF2 for passwords because there is no argon2 or scrypt,
// Ed25519 for assertions because it is native here and forty lines of Go on
// the other side, SHA-256 for everything that only has to be looked up.

const encoder = new TextEncoder();

// PASSWORD_ITERATIONS is OWASP's figure for PBKDF2-HMAC-SHA256, which is the
// strongest thing this runtime does natively. It is a real cost per sign-in —
// a few hundred milliseconds of CPU — which is why only signing in pays it and
// a client token is checked with a hash.
export const PASSWORD_ITERATIONS = 600_000;

// ROUND_ITERATIONS is the most this runtime will do in one call. Workers
// refuses a PBKDF2 request above 100,000 outright:
//
//   NotSupportedError: Pbkdf2 failed: iteration counts above 100000 are not
//   supported (requested 600000).
//
// workerd does not enforce the cap, so a suite that passes locally says
// nothing about this. The cap is a property of the platform, not advice about
// how much stretching a password deserves.
const ROUND_ITERATIONS = 100_000;

// So the work is done as a chain of rounds instead of one call. Each round's
// output is the next round's input, which costs an attacker the same
// 600,000 iterations and cannot be split across cores: round six cannot start
// until round five has finished. Lowering the count to fit the cap would have
// been the other option, and it would have quietly made every password in
// here six times cheaper to attack.
const ROUNDS = PASSWORD_ITERATIONS / ROUND_ITERATIONS;

/** random returns n bytes from the runtime's random source, base64url. */
export function random(n = 32) {
  const bytes = new Uint8Array(n);
  crypto.getRandomValues(bytes);
  return base64url(bytes);
}

/** id is a random identifier short enough to appear in a URL. */
export function id() {
  return random(16);
}

export function base64url(bytes) {
  let binary = "";
  for (const byte of new Uint8Array(bytes)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function fromBase64url(text) {
  const padded = text.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

/**
 * hash is how every presented secret is looked up.
 *
 * A token, a server secret and an invitation code are all 32 random bytes, so
 * there is nothing to stretch: an attacker who can guess one of these can
 * guess the output of a CSPRNG, and no amount of iteration helps with that.
 * Passwords are the exception and go through derive instead.
 */
export async function hash(secret) {
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(secret));
  return base64url(digest);
}

/**
 * derive stretches a password against its salt.
 *
 * PASSWORD_ITERATIONS in total, taken ROUND_ITERATIONS at a time because the
 * runtime will not do more than that in a single call. See the note on
 * ROUNDS: the chain is sequential, so the total work is the same as one call
 * of PASSWORD_ITERATIONS would have been.
 */
export async function derive(password, salt) {
  const saltBytes = fromBase64url(salt);
  let material = encoder.encode(password);
  for (let round = 0; round < ROUNDS; round++) {
    const key = await crypto.subtle.importKey("raw", material, "PBKDF2", false, ["deriveBits"]);
    const bits = await crypto.subtle.deriveBits(
      {
        name: "PBKDF2",
        hash: "SHA-256",
        salt: saltBytes,
        iterations: ROUND_ITERATIONS,
      },
      key,
      256,
    );
    material = new Uint8Array(bits);
  }
  return base64url(material);
}

/**
 * sameSecret compares two already-hashed values without leaking where they
 * first differ.
 *
 * The inputs are hashes, so they are a fixed length and comparing them is
 * safe to do in constant time; comparing the secrets themselves would not be.
 */
export function sameSecret(a, b) {
  if (typeof a !== "string" || typeof b !== "string" || a.length !== b.length) return false;
  let difference = 0;
  for (let i = 0; i < a.length; i++) difference |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return difference === 0;
}

// USER_CODE_ALPHABET has no 0, O, 1, I, S or 5 in it. The code is read off a
// television across a room and typed by somebody who is already mildly
// annoyed, and every one of those characters is a pair somebody mistypes.
const USER_CODE_ALPHABET = "ABCDEFGHJKLMNPQRTUVWXYZ2346789";

/** userCode is the eight characters shown on the television, in two groups. */
export function userCode() {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  let out = "";
  for (let i = 0; i < 8; i++) {
    if (i === 4) out += "-";
    out += USER_CODE_ALPHABET[bytes[i] % USER_CODE_ALPHABET.length];
  }
  return out;
}

/** normaliseUserCode accepts what somebody actually typed. */
export function normaliseUserCode(text) {
  const bare = String(text || "")
    .toUpperCase()
    .replace(/[^A-Z0-9]/g, "");
  if (bare.length !== 8) return "";
  return bare.slice(0, 4) + "-" + bare.slice(4);
}

/**
 * signAssertion produces the compact JWS a server verifies.
 *
 * The header names the key but never decides the algorithm: the verifier is
 * written to accept EdDSA and nothing else, which is what keeps `alg: none`
 * and its relatives off the table.
 */
export async function signAssertion(privateKeyPkcs8, kid, payload) {
  const key = await crypto.subtle.importKey(
    "pkcs8",
    fromBase64url(privateKeyPkcs8),
    { name: "Ed25519" },
    false,
    ["sign"],
  );
  const header = base64url(encoder.encode(JSON.stringify({ alg: "EdDSA", typ: "bask+jws", kid })));
  const body = base64url(encoder.encode(JSON.stringify(payload)));
  const signature = await crypto.subtle.sign(
    { name: "Ed25519" },
    key,
    encoder.encode(header + "." + body),
  );
  return header + "." + body + "." + base64url(signature);
}

/** publicKeyOf exports the public half of a stored private key. */
export async function publicKeyOf(privateKeyPkcs8) {
  // Ed25519's public key is the last 32 bytes of the seed's expansion, but
  // reading it out of the PKCS#8 by hand is the sort of cleverness that breaks
  // on an encoding change. Import, export as JWK, take the x coordinate.
  const key = await crypto.subtle.importKey(
    "pkcs8",
    fromBase64url(privateKeyPkcs8),
    { name: "Ed25519" },
    true,
    ["sign"],
  );
  const jwk = await crypto.subtle.exportKey("jwk", key);
  return jwk.x;
}
