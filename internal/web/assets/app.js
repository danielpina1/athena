// Athena's placeholder pages (plan 3); plan 4's dashboard replaces this file.
//
// /enroll creates a passkey with the single-use token in the URL fragment. / signs in with a passkey, then opens /ws
// and keeps a live count of the open Tickets, which proves the whole path from a phone.
//
// The wire is athena.v1: one protobuf-encoded Frame per binary WebSocket message. With no library (the CSP allows
// only this file), it hand-encodes the few frames it sends (Hello, Credit, Pong, a standalone ack) and decodes only
// what it reads: Welcome, Ping, Error, and from Snapshot and Delta each TicketSummary's id and ended_at.
"use strict";

const $ = (id) => document.getElementById(id);

// --- WebAuthn JSON: the server speaks base64url, the browser ArrayBuffers ---

function fromB64url(s) {
  const b = atob(s.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(b, (c) => c.charCodeAt(0)).buffer;
}

function toB64url(buf) {
  let s = "";
  for (const v of new Uint8Array(buf)) s += String.fromCharCode(v);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function credentialJSON(c) {
  const r = c.response;
  const out = { id: c.id, rawId: toB64url(c.rawId), type: c.type, response: { clientDataJSON: toB64url(r.clientDataJSON) } };
  if (r.attestationObject) out.response.attestationObject = toB64url(r.attestationObject);
  if (r.authenticatorData) {
    out.response.authenticatorData = toB64url(r.authenticatorData);
    out.response.signature = toB64url(r.signature);
    if (r.userHandle) out.response.userHandle = toB64url(r.userHandle);
  }
  return out;
}

async function post(path, body) {
  const r = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? "{}" : JSON.stringify(body),
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || "HTTP " + r.status);
  return data;
}

async function enroll(token, name) {
  const o = (await post("/auth/register/begin", { token, name })).publicKey;
  o.challenge = fromB64url(o.challenge);
  o.user.id = fromB64url(o.user.id);
  for (const c of o.excludeCredentials || []) c.id = fromB64url(c.id);
  const cred = await navigator.credentials.create({ publicKey: o });
  await post("/auth/register/finish", credentialJSON(cred));
}

async function login() {
  const o = (await post("/auth/login/begin")).publicKey;
  o.challenge = fromB64url(o.challenge);
  for (const c of o.allowCredentials || []) c.id = fromB64url(c.id);
  const cred = await navigator.credentials.get({ publicKey: o });
  await post("/auth/login/finish", credentialJSON(cred));
}

// --- protobuf, just enough ---

const CONTROL = 1, STREAM = 3;

function varint(n) {
  const out = [];
  while (n >= 0x80) {
    out.push((n % 0x80) | 0x80);
    n = Math.floor(n / 0x80);
  }
  out.push(n);
  return out;
}

const tag = (field, wire) => varint(field * 8 + wire);
const uintField = (field, n) => (n ? [...tag(field, 0), ...varint(n)] : []);
const bytesField = (field, bytes) => [...tag(field, 2), ...varint(bytes.length), ...bytes];
const strField = (field, s) => (s ? bytesField(field, [...new TextEncoder().encode(s)]) : []);

// frame is a Frame on CONTROL with body (field number, encoded message) and, when ack is set, a cumulative ack.
function frame(bodyField, body, ack) {
  return new Uint8Array([...uintField(1, CONTROL), ...uintField(3, ack), ...bytesField(bodyField, body)]);
}

// fields decodes one message into {field number: [values]}: numbers for varints, Uint8Arrays for length-delimited
// fields. Fixed-width fields are skipped; none of what is read here uses them.
function fields(buf) {
  const out = {};
  let i = 0;
  const readVarint = () => {
    let n = 0, mul = 1, b;
    do {
      if (i >= buf.length) throw new Error("truncated varint");
      b = buf[i++];
      n += (b & 0x7f) * mul;
      mul *= 0x80;
    } while (b & 0x80);
    return n;
  };
  while (i < buf.length) {
    const key = readVarint(), field = Math.floor(key / 8), wire = key % 8;
    let v;
    if (wire === 0) v = readVarint();
    else if (wire === 2) {
      const len = readVarint();
      v = buf.subarray(i, i + len);
      i += len;
    } else if (wire === 1) { i += 8; continue; }
    else if (wire === 5) { i += 4; continue; }
    else throw new Error("wire type " + wire);
    (out[field] = out[field] || []).push(v);
  }
  return out;
}

const one = (f, n) => (f[n] ? f[n][f[n].length - 1] : undefined);
const text = (b) => (b ? new TextDecoder().decode(b) : "");

// --- the link ---

// clientId names this device's commands to the server; it lasts as long as the browser's storage.
function clientId() {
  let id = "";
  try { id = localStorage.getItem("athena.clientId") || ""; } catch (e) { /* private mode: a fresh id */ }
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(id)) {
    id = "web-" + Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) => b.toString(16).padStart(2, "0")).join("");
    try { localStorage.setItem("athena.clientId", id); } catch (e) { /* ignore */ }
  }
  return id;
}

// connect keeps a connection open and calls show(openCount) on each change. State survives a drop through the hub
// cursor: the next Hello names the epoch and cursor, and the server sends the missed Deltas or a fresh Snapshot.
function connect(show, signedOut) {
  const open = new Map(); // ticket id -> open
  let epoch = "", cursor = 0, lastSeq = 0, ackTimer = 0;
  const id = clientId();

  const ticket = (b) => {
    const t = fields(b);
    open.set(text(one(t, 1)), one(t, 10) === undefined); // 1 id, 10 ended_at
  };
  const count = () => show([...open.values()].filter(Boolean).length);

  const dial = () => {
    const ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws", "athena.v1");
    ws.binaryType = "arraybuffer";
    lastSeq = 0; // each connection is a fresh link session: frames are numbered from 1 again
    const send = (bytes) => { if (ws.readyState === WebSocket.OPEN) ws.send(bytes); };
    ws.onopen = () => {
      // Hello {protocol 1, client_id, epoch, cursor}
      send(frame(10, [...uintField(1, 1), ...strField(2, id), ...strField(3, epoch), ...uintField(4, cursor)]));
      // Credit {channel STREAM, items}: a dashboard grants STREAM items as it reads them, on top of the 256 each
      // connection starts with. This page opens no stream; the grant is here to show the encoding.
      send(frame(14, [...uintField(1, STREAM), ...uintField(2, 256)]));
    };
    ws.onmessage = (ev) => {
      const f = fields(new Uint8Array(ev.data));
      const seq = one(f, 2) || 0;
      if (seq > lastSeq) {
        lastSeq = seq;
        // A standalone ack is a Ping with no time; without acks the server stops sending durable frames.
        clearTimeout(ackTimer);
        ackTimer = setTimeout(() => send(frame(12, [], lastSeq)), 200);
      }
      if (f[11]) epoch = text(one(fields(one(f, 11)), 1)); // Welcome.epoch
      if (f[12]) { // Ping {at_unix_ms}: answer with a Pong carrying the same time, so the server sees we are alive
        const at = one(fields(one(f, 12)), 1);
        if (at) send(frame(13, uintField(1, at), lastSeq));
      }
      if (f[15]) console.warn("athena:", text(one(fields(one(f, 15)), 2))); // Error.message
      if (f[19]) { // Snapshot {cursor 1, tickets 2}
        const s = fields(one(f, 19));
        open.clear();
        for (const t of s[2] || []) ticket(t);
        cursor = one(s, 1) || 0;
        count();
      }
      if (f[20]) { // Delta {seq 1, ticket 10}; a ticket_detail delta has seq 0 and does not move the cursor
        const d = fields(one(f, 20));
        if (one(d, 1)) cursor = one(d, 1);
        if (d[10]) { ticket(one(d, 10)); count(); }
      }
    };
    ws.onclose = (ev) => {
      clearTimeout(ackTimer);
      if (ev.code === 4002) { signedOut(); return; } // the session ended (revoked, logged out or expired)
      // Anything else (a restart, a network change, a refused upgrade) is retried while the session lasts.
      setTimeout(async () => {
        try {
          if (!(await (await fetch("/auth/session")).json()).authenticated) { signedOut(); return; }
        } catch (e) { /* the server is unreachable: try again */ }
        dial();
      }, 2000);
    };
  };
  dial();
}

// --- pages ---

async function indexPage() {
  const status = $("status");
  const signedIn = () => {
    status.textContent = "";
    $("login").hidden = true;
    $("signed-in").hidden = false;
    connect((n) => { $("count").textContent = String(n); }, () => location.reload());
  };
  $("login").onclick = async () => {
    status.textContent = "Waiting for the passkey…";
    try { await login(); signedIn(); } catch (e) { status.textContent = "Sign-in failed: " + e.message; }
  };
  $("logout").onclick = async () => {
    try { await post("/auth/logout"); } finally { location.reload(); }
  };
  try {
    const s = await (await fetch("/auth/session")).json();
    if (s.authenticated) return signedIn();
  } catch (e) { /* fall through to sign-in */ }
  status.textContent = "Not signed in.";
  $("login").hidden = false;
}

function enrollPage() {
  const status = $("status");
  const token = location.hash.slice(1);
  // The token stays out of the history and of anything the page might send besides the enrollment request.
  history.replaceState(null, "", location.pathname);
  if (!token) {
    status.textContent = "This page needs the link athena web enroll printed.";
    return;
  }
  const form = $("enroll-form");
  form.hidden = false;
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    form.hidden = true;
    status.textContent = "Waiting for the passkey…";
    try {
      await enroll(token, $("name").value);
      status.textContent = "Enrolled.";
      $("done").hidden = false;
    } catch (e) {
      status.textContent = "Enrollment failed: " + e.message + ". The link works once; run athena web enroll for a new one.";
    }
  };
}

if (document.body.dataset.page === "enroll") enrollPage();
else indexPage();
