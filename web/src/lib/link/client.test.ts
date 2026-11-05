import { create, toBinary } from "@bufbuild/protobuf";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { Channel, FrameSchema, WelcomeSchema } from "@/gen/athena/v1/link_pb";
import { FakeServer } from "./fakeServer";
import { LinkClient } from "./client";
import { decodeFrame, encodeFrame } from "./frame";
import { connectStore, useAthena } from "@/lib/store/state";

let server: FakeServer;
let client: LinkClient;
let statuses: string[];

function newClient() {
  client = new LinkClient({ url: "wss://athena.test/ws", clientId: "dev-1", WebSocketImpl: server.WebSocket });
  statuses = [];
  client.onStatus((s) => statuses.push(s));
  return client;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, "random").mockReturnValue(0.5); // jitter factor 1: the bare backoff
  server = new FakeServer();
  useAthena.getState().reset();
  newClient();
});

afterEach(() => {
  client.stop();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function setVisibility(state: "visible" | "hidden") {
  Object.defineProperty(document, "visibilityState", { value: state, configurable: true });
  document.dispatchEvent(new Event("visibilitychange"));
}

describe("frame", () => {
  test("round-trips a frame", () => {
    const f = create(FrameSchema, { channel: Channel.CONTROL, seq: 2n, ack: 9n, body: { case: "ping", value: { atUnixMs: 5n } } });
    const back = decodeFrame(encodeFrame(f));
    expect(back.seq).toBe(2n);
    expect(back.ack).toBe(9n);
    expect(back.body.case).toBe("ping");
  });

  test("tolerates unknown fields from a newer server", () => {
    const known = toBinary(FrameSchema, create(FrameSchema, { body: { case: "welcome", value: create(WelcomeSchema, { epoch: "e9" }) } }));
    // field 99, varint 1: (99 << 3) | 0 = 792 = 0x98 0x06
    const bytes = new Uint8Array([...known, 0x98, 0x06, 0x01]);
    const f = decodeFrame(bytes);
    expect(f.body.case).toBe("welcome");
    if (f.body.case === "welcome") expect(f.body.value.epoch).toBe("e9");
  });
});

describe("LinkClient", () => {
  test("opens athena.v1 with binary frames and says Hello first, not durable", () => {
    client.start();
    const s = server.socket;
    expect(s.url).toBe("wss://athena.test/ws");
    expect(s.protocols).toEqual(["athena.v1"]);
    expect(s.binaryType).toBe("arraybuffer");
    expect(statuses).toEqual(["connecting"]);
    s.accept();
    const [hello] = s.sent("hello");
    expect(hello).toMatchObject({ protocol: 1, clientId: "dev-1", epoch: "", cursor: 0n });
    expect(s.received[0].seq).toBe(0n);
    expect(s.received[0].channel).toBe(Channel.CONTROL);
    expect(s.violations).toEqual([]);
  });

  test("status is open only after Welcome", () => {
    client.start();
    server.socket.accept();
    expect(client.status).toBe("connecting");
    server.welcome();
    expect(client.status).toBe("open");
    expect(statuses).toEqual(["connecting", "open"]);
  });

  test("hello carries epoch and cursor after a snapshot", () => {
    const off = connectStore(client);
    client.start();
    const s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 7n } } });
    expect(useAthena.getState().epoch).toBe("e1");
    expect(useAthena.getState().cursor).toBe(7n);
    s.serverClose(1001, "server shutting down");
    vi.advanceTimersByTime(500);
    server.socket.accept();
    expect(server.socket.sent("hello")[0]).toMatchObject({ protocol: 1, clientId: "dev-1", epoch: "e1", cursor: 7n });
    off();
  });

  test("the Welcome's server version is kept for Settings › System", () => {
    const off = connectStore(client);
    client.start();
    server.welcome("e1", true);
    expect(useAthena.getState().serverVersion).toBe("test");
    off();
  });

  test("a Welcome's epoch counts only once its Snapshot is applied", () => {
    const off = connectStore(client);
    client.start();
    server.welcome("e1", true).push({ body: { case: "snapshot", value: { cursor: 7n } } });
    server.socket.serverClose(1001);
    vi.advanceTimersByTime(500);
    // A new epoch: the Snapshot that would replace the state never arrives before the drop.
    server.welcome("e2", true).serverClose(1006);
    vi.advanceTimersByTime(500);
    server.socket.accept();
    expect(server.socket.sent("hello")[0]).toMatchObject({ epoch: "e1", cursor: 7n });
    off();
  });

  test("answers ping with pong of the same time", () => {
    client.start();
    const s = server.welcome();
    s.push({ body: { case: "ping", value: { atUnixMs: 1730000000123n } } });
    expect(s.sent("pong")).toHaveLength(1);
    expect(s.sent("pong")[0].atUnixMs).toBe(1730000000123n);
    // A Ping at 0 is a standalone ack: nothing to answer.
    s.push({ body: { case: "ping", value: { atUnixMs: 0n } } });
    expect(s.sent("pong")).toHaveLength(1);
  });

  test("acks durable frames cumulatively", () => {
    client.start();
    const s = server.welcome();
    for (let i = 0; i < 3; i++) s.push({ body: { case: "commandResult", value: { id: `c${i}`, ok: true } } }, { durable: true });
    // Nothing else to carry the ack: a standalone ack goes out within 1 s.
    vi.advanceTimersByTime(1000);
    const acks = s.received.filter((f) => f.body.case === "ping");
    expect(acks).toHaveLength(1);
    expect(acks[0].ack).toBe(3n);
    if (acks[0].body.case === "ping") expect(acks[0].body.value.atUnixMs).toBe(0n);
    // Nothing new: no further ack.
    vi.advanceTimersByTime(5000);
    expect(s.received.filter((f) => f.body.case === "ping")).toHaveLength(1);

    // A durable frame from the client is numbered from 1 and carries the ack.
    s.push({ body: { case: "commandResult", value: { id: "c3", ok: true } } }, { durable: true });
    client.send(Channel.CONTROL, true, { case: "command", value: { id: "x1", action: { case: "stop", value: { ticketId: "T-1" } } } });
    client.send(Channel.CONTROL, true, { case: "command", value: { id: "x2", action: { case: "stop", value: { ticketId: "T-2" } } } });
    client.send(Channel.CONTROL, false, { case: "resync", value: {} });
    const out = s.received.slice(-3);
    expect(out.map((f) => f.seq)).toEqual([1n, 2n, 0n]);
    expect(out.map((f) => f.ack)).toEqual([4n, 4n, 4n]);
    vi.advanceTimersByTime(1000);
    expect(s.received.filter((f) => f.body.case === "ping")).toHaveLength(1); // the commands carried it

    // A Pong carries the ack too; a repeated frame is acked again, not delivered twice.
    const delivered: string[] = [];
    client.onFrame((f) => delivered.push(f.body.case ?? ""));
    s.push({ seq: 4n, body: { case: "commandResult", value: { id: "c3", ok: true } } });
    expect(delivered).toEqual([]);
    vi.advanceTimersByTime(1000);
    expect(s.received.filter((f) => f.body.case === "ping")).toHaveLength(2);
    expect(s.received.at(-1)?.ack).toBe(4n);
    expect(s.violations).toEqual([]);
  });

  test("frames are numbered per connection", () => {
    client.start();
    let s = server.welcome();
    client.send(Channel.CONTROL, true, { case: "command", value: { id: "a" } });
    s.serverClose(1006);
    vi.advanceTimersByTime(500);
    s = server.welcome();
    client.send(Channel.CONTROL, true, { case: "command", value: { id: "b" } });
    expect(s.received.at(-1)?.seq).toBe(1n);
    expect(s.received.at(-1)?.ack).toBe(0n);
    expect(s.violations).toEqual([]);
  });

  test("send before Welcome is refused", () => {
    client.start();
    server.socket.accept();
    expect(client.send(Channel.CONTROL, true, { case: "command", value: { id: "a" } })).toBe(false);
    expect(server.socket.sent("command")).toHaveLength(0);
  });

  test("a sequence gap from the server drops the connection", () => {
    client.start();
    const s = server.welcome();
    s.push({ seq: 2n, body: { case: "commandResult", value: { id: "c" } } });
    expect(s.closedByClient?.code).toBe(4001);
    expect(client.status).toBe("offline");
    vi.advanceTimersByTime(500);
    expect(server.sockets).toHaveLength(2);
  });

  test("reconnects with backoff and jitter, resets after 10 s open", () => {
    client.start();
    const delays = [500, 1000, 2000, 4000, 8000, 15000, 15000];
    for (const d of delays) {
      const n = server.sockets.length;
      server.socket.serverClose(1006);
      expect(client.status).toBe("offline");
      vi.advanceTimersByTime(d - 1);
      expect(server.sockets).toHaveLength(n);
      vi.advanceTimersByTime(1);
      expect(server.sockets).toHaveLength(n + 1);
    }

    // Jitter is ±20 %.
    vi.mocked(Math.random).mockReturnValue(0);
    let n = server.sockets.length;
    server.socket.serverClose(1006);
    vi.advanceTimersByTime(12000 - 1);
    expect(server.sockets).toHaveLength(n);
    vi.advanceTimersByTime(1);
    expect(server.sockets).toHaveLength(n + 1);
    vi.mocked(Math.random).mockReturnValue(0.999999);
    n = server.sockets.length;
    server.socket.serverClose(1006);
    vi.advanceTimersByTime(17999);
    expect(server.sockets).toHaveLength(n);
    vi.advanceTimersByTime(1);
    expect(server.sockets).toHaveLength(n + 1);

    // Open for less than 10 s: no reset.
    vi.mocked(Math.random).mockReturnValue(0.5);
    server.welcome();
    vi.advanceTimersByTime(9000);
    n = server.sockets.length;
    server.socket.serverClose(1001);
    vi.advanceTimersByTime(14999);
    expect(server.sockets).toHaveLength(n);
    vi.advanceTimersByTime(1);
    expect(server.sockets).toHaveLength(n + 1);

    // Open for 10 s: back to 0.5 s.
    server.welcome();
    vi.advanceTimersByTime(10000);
    n = server.sockets.length;
    server.socket.serverClose(1001);
    vi.advanceTimersByTime(499);
    expect(server.sockets).toHaveLength(n);
    vi.advanceTimersByTime(1);
    expect(server.sockets).toHaveLength(n + 1);
  });

  test("reconnects at once on online and visible", () => {
    client.start();
    server.welcome();
    for (let i = 0; i < 4; i++) {
      server.socket.serverClose(1006);
      vi.runOnlyPendingTimers();
    }
    server.socket.serverClose(1006);
    let n = server.sockets.length;
    window.dispatchEvent(new Event("online"));
    expect(server.sockets).toHaveLength(n + 1);
    // Already connecting: another wake opens nothing more.
    window.dispatchEvent(new Event("online"));
    expect(server.sockets).toHaveLength(n + 1);

    server.socket.serverClose(1006);
    n = server.sockets.length;
    setVisibility("hidden");
    expect(server.sockets).toHaveLength(n);
    setVisibility("visible");
    expect(server.sockets).toHaveLength(n + 1);
    // The backoff timer that was pending does not open a second socket (within the 45 s a connect may take).
    vi.advanceTimersByTime(30000);
    expect(server.sockets).toHaveLength(n + 1);
  });

  test("a socket silent past 45 s is replaced on visible", () => {
    client.start();
    const s = server.welcome();
    // Timers in a hidden tab may not run; the clock still moves.
    vi.setSystemTime(Date.now() + 50000);
    setVisibility("visible");
    expect(s.closedByClient?.code).toBe(4000);
    expect(server.sockets).toHaveLength(2);
  });

  test("drops the connection after 45 s without a frame", () => {
    client.start();
    const s = server.welcome();
    for (let i = 0; i < 4; i++) {
      vi.advanceTimersByTime(15000);
      s.push({ body: { case: "ping", value: { atUnixMs: BigInt(i + 1) } } });
    }
    expect(s.closedByClient).toBeUndefined();
    vi.advanceTimersByTime(44999);
    expect(s.closedByClient).toBeUndefined();
    vi.advanceTimersByTime(1);
    expect(s.closedByClient?.code).toBe(4000);
    expect(client.status).toBe("offline");
    vi.advanceTimersByTime(500);
    expect(server.sockets).toHaveLength(2);
  });

  test("a connect that never completes is dropped after 45 s", () => {
    client.start();
    const s = server.socket; // never accepted
    vi.advanceTimersByTime(44999);
    expect(s.closedByClient).toBeUndefined();
    vi.advanceTimersByTime(1);
    expect(s.closedByClient?.code).toBe(4000);
    vi.advanceTimersByTime(500);
    expect(server.sockets).toHaveLength(2);
  });

  test("close 4002 sets signedOut and stops reconnecting", () => {
    client.start();
    server.welcome().serverClose(4002, "session ended");
    expect(client.status).toBe("signedOut");
    expect(statuses.at(-1)).toBe("signedOut");
    vi.advanceTimersByTime(120000);
    window.dispatchEvent(new Event("online"));
    setVisibility("visible");
    expect(server.sockets).toHaveLength(1);
    client.stop();
    expect(client.status).toBe("signedOut");
    // Signing in again starts it afresh.
    client.start();
    expect(server.sockets).toHaveLength(2);
    expect(client.status).toBe("connecting");
  });

  test("1013 reconnects with the cursor", () => {
    const off = connectStore(client);
    client.start();
    const s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 5n } } });
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 6n, change: { case: "ticket", value: { id: "T-1" } } } } });
    s.serverClose(1013, "too far behind");
    expect(client.status).toBe("offline");
    vi.advanceTimersByTime(500);
    server.socket.accept();
    expect(server.socket.sent("hello")[0]).toMatchObject({ epoch: "e1", cursor: 6n });
    off();
  });

  test("4001 waits the longest backoff; 1001, 1006, 1011 and 4000 reconnect", () => {
    client.start();
    server.welcome().serverClose(4001, "protocol violation");
    vi.advanceTimersByTime(14999);
    expect(server.sockets).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(server.sockets).toHaveLength(2);
    for (const code of [1001, 1006, 1011, 4000]) {
      const n = server.sockets.length;
      server.welcome().serverClose(code);
      vi.advanceTimersByTime(15000);
      expect(server.sockets, `after ${code}`).toHaveLength(n + 1);
      expect(client.status).toBe("connecting");
    }
  });

  test("4001 after a stable connection still waits the longest backoff", () => {
    client.start();
    server.welcome();
    vi.advanceTimersByTime(10000);
    server.socket.serverClose(4001, "protocol violation");
    vi.advanceTimersByTime(14999);
    expect(server.sockets).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(server.sockets).toHaveLength(2);
  });

  test("sends no stream credit on (re)connect; grants carry what the view passes", () => {
    client.start();
    let s = server.welcome();
    expect(s.sent("credit")).toEqual([]);
    client.grantStream(64);
    expect(s.sent("credit")).toHaveLength(1);
    expect(s.sent("credit")[0]).toMatchObject({ channel: Channel.STREAM, items: 64 });
    const f = s.received.at(-1);
    expect(f?.channel).toBe(Channel.CONTROL);
    expect(f?.seq).toBe(0n);
    s.serverClose(1006);
    client.grantStream(10); // offline: credit does not carry over, nothing to send
    vi.advanceTimersByTime(500);
    s = server.welcome();
    expect(s.sent("credit")).toEqual([]);
    client.grantStream(0);
    expect(s.sent("credit")).toEqual([]);
  });

  test("delta gap triggers resync", () => {
    const off = connectStore(client);
    client.start();
    const s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 5n } } });
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 7n, change: { case: "ticket", value: { id: "T-7" } } } } });
    expect(s.sent("resync")).toHaveLength(1);
    expect(useAthena.getState().cursor).toBe(5n);
    expect(useAthena.getState().tickets.has("T-7")).toBe(false);
    // Asked once: later deltas before the Snapshot do not ask again.
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 8n, change: { case: "ticket", value: { id: "T-8" } } } } });
    expect(s.sent("resync")).toHaveLength(1);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 8n, tickets: [{ id: "T-8" }] } } });
    expect(useAthena.getState().cursor).toBe(8n);
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 9n, change: { case: "ticket", value: { id: "T-9" } } } } });
    expect(useAthena.getState().cursor).toBe(9n);
    expect([...useAthena.getState().tickets.keys()].sort()).toEqual(["T-8", "T-9"]);
    // A later gap asks again.
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 11n, change: { case: "ticket", value: { id: "T-11" } } } } });
    expect(s.sent("resync")).toHaveLength(2);
    off();
  });

  test("seq 0 detail does not move cursor", () => {
    const off = connectStore(client);
    client.start();
    const s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 5n } } });
    s.push({
      channel: Channel.EVENTS,
      body: { case: "delta", value: { seq: 0n, change: { case: "ticketDetail", value: { summary: { id: "T-1", title: "x" } } } } },
    });
    expect(useAthena.getState().cursor).toBe(5n);
    expect(useAthena.getState().details.get("T-1")?.summary?.title).toBe("x");
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 6n, change: { case: "ticket", value: { id: "T-1" } } } } });
    expect(useAthena.getState().cursor).toBe(6n);
    expect(s.sent("resync")).toEqual([]);
    off();
  });

  test("a throwing subscriber is reported and the link still opens", () => {
    const errors: unknown[] = [];
    client = new LinkClient({ url: "wss://athena.test/ws", clientId: "dev-1", WebSocketImpl: server.WebSocket, reportError: (e) => errors.push(e) });
    const seen: string[] = [];
    client.onFrame(() => {
      throw new Error("frame subscriber");
    });
    client.onFrame((f) => seen.push(f.body.case ?? ""));
    client.onStatus(() => {
      throw new Error("status subscriber");
    });
    client.start();
    server.welcome();
    expect(client.status).toBe("open");
    expect(seen).toEqual(["welcome"]);
    expect(errors.map((e) => (e as Error).message)).toEqual(["status subscriber", "frame subscriber", "status subscriber"]);
  });

  test("a durable seq is not spent when the write fails", () => {
    client.start();
    const s = server.welcome();
    const send = vi.spyOn(s, "send").mockImplementationOnce(() => {
      throw new Error("socket gone");
    });
    expect(client.send(Channel.CONTROL, true, { case: "command", value: { id: "a" } })).toBe(false);
    send.mockRestore();
    expect(client.send(Channel.CONTROL, true, { case: "command", value: { id: "b" } })).toBe(true);
    expect(s.received.at(-1)?.seq).toBe(1n);
    expect(s.violations).toEqual([]);
  });

  test("a store reset resumes from nothing", () => {
    const off = connectStore(client);
    client.start();
    const s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 7n } } });
    useAthena.getState().reset(); // sign-out
    s.serverClose(1006);
    vi.advanceTimersByTime(500);
    server.socket.accept();
    expect(server.socket.sent("hello")[0]).toMatchObject({ epoch: "", cursor: 0n });
    off();
  });

  test("a resume without a Snapshot applies the missed Deltas in order", () => {
    const off = connectStore(client);
    client.start();
    let s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 5n, tickets: [{ id: "T-1", title: "kept" }] } } });
    s.serverClose(1001);
    vi.advanceTimersByTime(500);
    s = server.welcome("e1", false);
    expect(s.sent("hello")[0]).toMatchObject({ epoch: "e1", cursor: 5n });
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 6n, change: { case: "ticket", value: { id: "T-2", title: "a" } } } } });
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 7n, change: { case: "ticket", value: { id: "T-2", title: "b" } } } } });
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 8n, change: { case: "ticket", value: { id: "T-3" } } } } });
    const st = useAthena.getState();
    expect(st.epoch).toBe("e1");
    expect(st.cursor).toBe(8n);
    expect(st.tickets.get("T-1")?.title).toBe("kept");
    expect(st.tickets.get("T-2")?.title).toBe("b");
    expect(st.tickets.has("T-3")).toBe(true);
    expect(s.sent("resync")).toEqual([]);
    off();
  });

  test("a Snapshot the server sends unasked replaces the state", () => {
    const off = connectStore(client);
    client.start();
    const s = server.welcome("e1", true);
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 5n, tickets: [{ id: "T-1" }] } } });
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 6n, change: { case: "ticket", value: { id: "T-2" } } } } });
    // The hub dropped this connection's feed for falling behind: a fresh Snapshot, no Welcome, no Resync asked.
    s.push({ channel: Channel.EVENTS, body: { case: "snapshot", value: { cursor: 40n, tickets: [{ id: "T-9" }] } } });
    expect(useAthena.getState().epoch).toBe("e1");
    expect(useAthena.getState().cursor).toBe(40n);
    expect([...useAthena.getState().tickets.keys()]).toEqual(["T-9"]);
    s.push({ channel: Channel.EVENTS, body: { case: "delta", value: { seq: 41n, change: { case: "ticket", value: { id: "T-10" } } } } });
    expect(useAthena.getState().cursor).toBe(41n);
    expect(s.sent("resync")).toEqual([]);
    s.serverClose(1001);
    vi.advanceTimersByTime(500);
    server.socket.accept();
    expect(server.socket.sent("hello")[0]).toMatchObject({ epoch: "e1", cursor: 41n });
    off();
  });

  test("stop closes and stays down", () => {
    client.start();
    const s = server.welcome();
    client.stop();
    expect(s.closedByClient?.code).toBe(1000);
    expect(client.status).toBe("offline");
    vi.advanceTimersByTime(60000);
    window.dispatchEvent(new Event("online"));
    expect(server.sockets).toHaveLength(1);
  });
});
