import { create } from "@bufbuild/protobuf";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { CommandResultSchema, type Command, type CommandResult } from "@/gen/athena/v1/commands_pb";
import { LinkClient } from "@/lib/link/client";
import { FakeServer } from "@/lib/link/fakeServer";
import { useAthena } from "@/lib/store/state";
import { newCommandId } from "./ids";
import { connectOutbox, EXPIRE_AFTER_MS, linkSender, Outbox, type Action } from "./outbox";

let idb: IDBFactory;

beforeEach(() => {
  idb = new IDBFactory();
  useAthena.getState().reset();
});

const ok = (id: string) => create(CommandResultSchema, { id, ok: true });
const fail = (id: string, code: string) => create(CommandResultSchema, { id, ok: false, code });
const reply = (text = "hi"): Action => ({ case: "reply", value: { ticketId: "T-1", text } });

/** A link the test opens and closes by hand: it records what each connection sent. */
class FakeLink {
  open = false;
  conn = 0;
  readonly sent: { conn: number; cmd: Command }[] = [];
  send = (cmd: Command): boolean => {
    if (!this.open) return false;
    this.sent.push({ conn: this.conn, cmd });
    return true;
  };
  /** The ids sent on connection n, in order. */
  ids(n = this.conn): string[] {
    return this.sent.filter((s) => s.conn === n).map((s) => s.cmd.id);
  }
  actions(n = this.conn): Action[] {
    return this.sent.filter((s) => s.conn === n).map((s) => s.cmd.action);
  }
  /** A new connection reaches Welcome; the outbox hears it. */
  connect(outbox: Outbox): void {
    this.conn++;
    this.open = true;
    outbox.onOpen();
  }
  drop(): void {
    this.open = false;
  }
}

/** The result a submit resolved with, once it has. "Once" is checked on the outbox's side effects instead. */
function track(p: Promise<CommandResult>) {
  const t = { result: undefined as CommandResult | undefined };
  void p.then((r) => (t.result = r));
  return t;
}

type Rec = { id: string };

/**
 * real, with a log of what IndexedDB committed: the ids put and deleted, each logged when its transaction completes
 * (not when it was asked for), so a test can tell what was durable at a given moment.
 */
function spied(real: IDBFactory) {
  const log = { committed: new Set<string>(), deleted: [] as string[] };
  const bind = <T extends object>(t: T, p: string | symbol) => {
    const v = Reflect.get(t, p, t);
    return typeof v === "function" ? v.bind(t) : v;
  };
  const wrapTx = (tx: IDBTransaction) => {
    const done: (() => void)[] = [];
    const store = (st: IDBObjectStore) =>
      new Proxy(st, {
        get(t, p) {
          if (p === "put") return (rec: Rec) => (done.push(() => log.committed.add(rec.id)), t.put(rec));
          if (p === "delete") return (id: string) => (done.push(() => log.deleted.push(id)), t.delete(id));
          return bind(t, p);
        },
      });
    return new Proxy(tx, {
      get: (t, p) => (p === "objectStore" ? (n: string) => store(t.objectStore(n)) : bind(t, p)),
      set(t, p, v) {
        if (p !== "oncomplete") return Reflect.set(t, p, v, t);
        t.oncomplete = (ev) => {
          for (const f of done) f();
          v.call(t, ev);
        };
        return true;
      },
    });
  };
  const wrapDb = (db: IDBDatabase) =>
    new Proxy(db, {
      get: (t, p) =>
        p === "transaction" ? (...a: Parameters<IDBDatabase["transaction"]>) => wrapTx(t.transaction(...a)) : bind(t, p),
      set: (t, p, v) => Reflect.set(t, p, v, t),
    });
  const factory = {
    open(name: string, version?: number) {
      const req = real.open(name, version);
      return new Proxy(req, {
        get: (t, p) => (p === "result" ? wrapDb(t.result) : bind(t, p)),
        set: (t, p, v) => Reflect.set(t, p, v, t),
      });
    },
  } as unknown as IDBFactory;
  return { factory, ...log };
}

describe("Outbox", () => {
  let server: FakeServer;
  let client: LinkClient;

  afterEach(() => {
    client?.stop();
  });

  test("outbox resends by id after reconnect and resolves once", async () => {
    server = new FakeServer();
    client = new LinkClient({ url: "wss://athena.test/ws", clientId: "dev-1", WebSocketImpl: server.WebSocket });
    const db = spied(idb);
    const outbox = new Outbox(db.factory, linkSender(client));
    const off = connectOutbox(client, outbox);
    client.start();
    const s1 = server.welcome();

    const approve: Action = { case: "answerApproval", value: { approvalId: "A-1", approve: true } };
    const commit = vi.fn();
    const rollback = vi.fn();
    const t = track(outbox.submit(approve, { commit, rollback }));
    await outbox.settled();
    const [first] = s1.sent("command");
    expect(first.action).toMatchObject(approve);
    expect(first.id).toHaveLength(26);

    // The phone loses the network before the result: nothing fails, the command waits.
    s1.serverClose(1006);
    await outbox.settled();
    expect(t.result).toBeUndefined();
    expect(outbox.pending().map((c) => c.id)).toEqual([first.id]);

    // The network comes back: the next connection carries the same command once, by the same id.
    window.dispatchEvent(new Event("online"));
    const s2 = server.welcome();
    await outbox.settled();
    expect(s1.sent("command").map((c) => c.id)).toEqual([first.id]);
    expect(s2.sent("command").map((c) => c.id)).toEqual([first.id]);
    expect(s2.violations).toEqual([]);
    expect(t.result).toBeUndefined();

    // The server answers the resend from its stored result; a repeat of it (a late answer to the first send) is
    // ignored. One result, never "failed" and never a second approve.
    const res = { body: { case: "commandResult", value: { id: first.id, ok: true } } } as const;
    s2.push(res, { durable: true });
    s2.push(res, { durable: true });
    await outbox.settled();
    expect(t.result).toMatchObject({ id: first.id, ok: true });
    expect(commit).toHaveBeenCalledTimes(1);
    expect(rollback).not.toHaveBeenCalled();
    expect(db.deleted).toEqual([first.id]);
    expect(outbox.pending()).toEqual([]);

    // Answered means gone: a third connection sends nothing, and a reload finds nothing.
    s2.serverClose(1006);
    window.dispatchEvent(new Event("online"));
    const s3 = server.welcome();
    await outbox.settled();
    expect(s3.sent("command")).toEqual([]);
    const reloaded = new Outbox(idb, () => true);
    await reloaded.settled();
    expect(reloaded.pending()).toEqual([]);
    off();
  });

  test("a command submitted offline is sent once on the next open", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    const commit = vi.fn();
    void outbox.submit(reply(), { commit });
    await outbox.settled();
    expect(link.sent).toEqual([]);
    link.connect(outbox);
    await outbox.settled();
    const [id] = link.ids();
    expect(link.ids()).toHaveLength(1);
    outbox.onResult(ok(id));
    outbox.onResult(ok(id));
    await outbox.settled();
    expect(commit).toHaveBeenCalledTimes(1);
  });

  test("outbox survives a page reload", async () => {
    const before = new Outbox(idb, () => false); // offline: never sent
    void before.submit(reply("first"));
    void before.submit(reply("second"));
    await before.settled();
    const ids = before.pending().map((c) => c.id);
    expect(ids).toHaveLength(2);

    // The page reloads: a new Outbox on the same database resends both, in id order, with the same ids.
    const link = new FakeLink();
    const after = new Outbox(idb, link.send);
    link.connect(after);
    await after.settled();
    expect(link.ids()).toEqual(ids);
    expect(link.actions()).toMatchObject([reply("first"), reply("second")]);

    after.onResult(ok(ids[0]));
    after.onResult(fail(ids[1], "not_found"));
    await after.settled();
    expect(after.pending()).toEqual([]);
    const again = new Outbox(idb, () => true);
    await again.settled();
    expect(again.pending()).toEqual([]);
  });

  test("reauth_required keeps and resends after step-up", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    let stepUps = 0;
    let finishStepUp!: (ok: boolean) => void;
    outbox.setReauthHandler(() => {
      stepUps++;
      return new Promise<boolean>((r) => (finishStepUp = r));
    });
    link.connect(outbox);
    const commit = vi.fn();
    const t = track(outbox.submit({ case: "setPolicy", value: { baseRevision: 3, settingsJson: "{}" } }, { commit }));
    await outbox.settled();
    const [id] = link.ids();

    outbox.onResult(fail(id, "reauth_required"));
    await outbox.settled();
    expect(stepUps).toBe(1);
    expect(t.result).toBeUndefined();
    expect(outbox.pending().map((c) => c.id)).toEqual([id]);
    // While the owner is at the passkey prompt the command is not resent, not even by a reconnect.
    link.connect(outbox);
    await outbox.settled();
    expect(link.ids()).toEqual([]);

    finishStepUp(true);
    await outbox.settled();
    expect(link.ids()).toEqual([id]);
    outbox.onResult(ok(id));
    outbox.onResult(ok(id));
    await outbox.settled();
    expect(t.result?.ok).toBe(true);
    expect(commit).toHaveBeenCalledTimes(1);
    expect(stepUps).toBe(1);
  });

  test("a step-up the owner cancels fails the command", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    outbox.setReauthHandler(async () => false);
    link.connect(outbox);
    const rollback = vi.fn();
    const t = track(outbox.submit({ case: "revokePasskey", value: { id: "P-1" } }, { rollback }));
    await outbox.settled();
    outbox.onResult(fail(link.ids()[0], "reauth_required"));
    await outbox.settled();
    expect(t.result).toMatchObject({ ok: false, code: "reauth_required" });
    expect(rollback).toHaveBeenCalledTimes(1);
    expect(outbox.pending()).toEqual([]);
  });

  test("unauthenticated keeps the command, signs out and resends after sign-in", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    const signOut = vi.fn();
    outbox.setSignOutHandler(signOut);
    link.connect(outbox);
    const commit = vi.fn();
    const t = track(outbox.submit(reply(), { commit }));
    await outbox.settled();
    const [id] = link.ids();
    outbox.onResult(fail(id, "unauthenticated"));
    await outbox.settled();
    expect(signOut).toHaveBeenCalledTimes(1);
    expect(t.result).toBeUndefined();
    expect(link.ids()).toEqual([id]); // not resent on the connection that refused it
    link.drop();
    link.connect(outbox); // signed in again
    await outbox.settled();
    expect(link.ids()).toEqual([id]);
    outbox.onResult(ok(id));
    await outbox.settled();
    expect(t.result?.ok).toBe(true);
    expect(commit).toHaveBeenCalledTimes(1);
  });

  test("failure rolls back the optimistic change", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    link.connect(outbox);
    const order: string[] = [];
    const t = track(
      outbox.submit(reply(), {
        optimistic: (s) => order.push(`optimistic:${s.tickets.size}`),
        rollback: (s) => order.push(`rollback:${s.tickets.size}`),
      }),
    );
    expect(order).toEqual(["optimistic:0"]); // at once, before any send
    await outbox.settled();
    outbox.onResult(fail(link.ids()[0], "conflict"));
    outbox.onResult(fail(link.ids()[0], "conflict")); // a repeat rolls back nothing more
    await outbox.settled();
    expect(order).toEqual(["optimistic:0", "rollback:0"]);
    expect(t.result).toMatchObject({ ok: false, code: "conflict" });
    expect(outbox.pending()).toEqual([]);
  });

  test("success keeps the optimistic change", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    link.connect(outbox);
    const rollback = vi.fn();
    void outbox.submit(reply(), { optimistic: () => {}, rollback });
    await outbox.settled();
    outbox.onResult(ok(link.ids()[0]));
    await outbox.settled();
    expect(rollback).not.toHaveBeenCalled();
  });

  test("commands older than 23 h are dropped with a notice", async () => {
    let now = Date.UTC(2026, 10, 4, 8, 0, 0);
    const notify = vi.fn();
    const offline = new Outbox(idb, () => false, { now: () => now, notify });
    const rollback = vi.fn();
    const t = track(offline.submit(reply("old"), { rollback }));
    await offline.settled();

    // 22 h on, a reload still sends it.
    now += 22 * 3600_000;
    const link = new FakeLink();
    const reload = new Outbox(idb, link.send, { now: () => now, notify });
    link.connect(reload);
    await reload.settled();
    expect(link.ids()).toHaveLength(1);
    expect(notify).not.toHaveBeenCalled();

    // Past 23 h the server may have forgotten the result: resending could act twice, so the command is dropped.
    now = Date.UTC(2026, 10, 4, 8, 0, 0) + EXPIRE_AFTER_MS;
    link.drop();
    link.connect(reload);
    link.drop();
    offline.onOpen();
    await reload.settled();
    await offline.settled();
    expect(link.ids()).toEqual([]);
    expect(reload.pending()).toEqual([]);
    expect(offline.pending()).toEqual([]);
    expect(notify).toHaveBeenCalledTimes(2); // once per outbox that dropped it
    expect(notify.mock.calls[0][0]).toMatch(/not sent/i);
    expect(t.result).toMatchObject({ ok: false, code: "expired" });
    expect(rollback).toHaveBeenCalledTimes(1);

    const fresh = new Outbox(idb, () => true, { now: () => now, notify });
    await fresh.settled();
    expect(fresh.pending()).toEqual([]);
  });

  test("watch is re-sent after reconnect", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    link.connect(outbox);
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-1" } });
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-2" } });
    await outbox.settled();
    const [w1, w2] = link.ids();
    outbox.onResult(ok(w1));
    // T-2's watch has no result yet: it is resent by id, not doubled by a fresh watch.
    expect(outbox.watched()).toEqual(["T-1", "T-2"]);

    link.drop();
    link.connect(outbox);
    await outbox.settled();
    const second = link.sent.filter((s) => s.conn === link.conn).map((s) => s.cmd);
    expect(second.map((c) => c.action)).toEqual([
      { case: "watchTicket", value: expect.objectContaining({ ticketId: "T-2" }) },
      { case: "watchTicket", value: expect.objectContaining({ ticketId: "T-1" }) },
    ]);
    expect(second[0].id).toBe(w2);
    expect(second[1].id).not.toBe(w1);
    for (const c of second) outbox.onResult(ok(c.id));

    // Unwatched, it is not re-sent; a watch the server refused is not either.
    void outbox.submit({ case: "unwatchTicket", value: { ticketId: "T-1" } });
    await outbox.settled();
    outbox.onResult(ok(link.ids().at(-1)!));
    link.drop();
    link.connect(outbox);
    await outbox.settled();
    const [rewatch] = link.sent.filter((s) => s.conn === link.conn).map((s) => s.cmd);
    expect(link.actions()).toEqual([{ case: "watchTicket", value: expect.objectContaining({ ticketId: "T-2" }) }]);
    outbox.onResult(fail(rewatch.id, "not_found"));
    expect(outbox.watched()).toEqual([]);
    link.drop();
    link.connect(outbox);
    await outbox.settled();
    expect(link.ids()).toEqual([]);

    // Watching belongs to the connection: it is never written to IndexedDB.
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-3" } });
    await outbox.settled();
    const reloaded = new Outbox(idb, () => true);
    await reloaded.settled();
    expect(reloaded.pending()).toEqual([]);
  });

  test("the last visibility is reported again after reconnect", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    link.connect(outbox);
    void outbox.submit({ case: "visibility", value: { visible: true } });
    await outbox.settled();
    outbox.onResult(ok(link.ids()[0]));
    link.drop();
    link.connect(outbox);
    await outbox.settled();
    expect(link.actions()).toEqual([{ case: "visibility", value: expect.objectContaining({ visible: true }) }]);
  });

  test("clear forgets every command and resolves their promises", async () => {
    const outbox = new Outbox(idb, () => false);
    const rollback = vi.fn();
    const t = track(outbox.submit(reply(), { rollback }));
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-1" } });
    await outbox.settled();
    outbox.clear();
    await outbox.settled();
    expect(outbox.pending()).toEqual([]);
    expect(outbox.watched()).toEqual([]);
    expect(t.result).toMatchObject({ ok: false, code: "unauthenticated" });
    expect(rollback).toHaveBeenCalledTimes(1);
    const reloaded = new Outbox(idb, () => true);
    await reloaded.settled();
    expect(reloaded.pending()).toEqual([]);
  });

  test("a command that ends this session and was sent is dropped when the session ends, never replayed", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    link.connect(outbox);
    const revoke = track(outbox.submit({ case: "revokePasskey", value: { id: "P-me" } }, { endsSession: true }));
    const other = track(outbox.submit(reply()));
    await outbox.settled();
    link.drop();
    // The session ended before either result came.
    outbox.dropSessionEnders();
    await outbox.settled();
    expect(revoke.result).toMatchObject({ ok: false, code: "signed_out" });
    expect(other.result).toBeUndefined();
    link.connect(outbox);
    await outbox.settled();
    expect(link.actions().map((a) => a?.case)).toEqual(["reply"]);
    const reloaded = new Outbox(idb, () => true);
    await reloaded.settled();
    expect(reloaded.pending().map((c) => c.action.case)).toEqual(["reply"]);
  });

  const revokeMe: Action = { case: "revokePasskey", value: { id: "P-me" } };

  test("a session ender held for a step-up when the session ends did not happen: not_done", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    outbox.setReauthHandler(() => new Promise<boolean>(() => {}));
    link.connect(outbox);
    const revoke = track(outbox.submit(revokeMe, { endsSession: true }));
    await outbox.settled();
    outbox.onResult(fail(link.ids()[0], "reauth_required"));
    outbox.dropSessionEnders();
    await outbox.settled();
    expect(revoke.result).toMatchObject({ ok: false, code: "not_done" });
    expect(outbox.pending()).toEqual([]);
  });

  test("a session ender answered unauthenticated did not happen: not_done", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    // The app's sign-out handler is leave(), which drops the session enders.
    outbox.setSignOutHandler(() => outbox.dropSessionEnders());
    link.connect(outbox);
    const revoke = track(outbox.submit(revokeMe, { endsSession: true }));
    await outbox.settled();
    outbox.onResult(fail(link.ids()[0], "unauthenticated"));
    await outbox.settled();
    expect(revoke.result).toMatchObject({ ok: false, code: "not_done" });
    expect(outbox.pending()).toEqual([]);
  });

  test("a session ender never sent before the session ended did not happen: not_done", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    // Offline: queued, never on the wire; the reconnect then ends in 4002 before Welcome.
    const revoke = track(outbox.submit(revokeMe, { endsSession: true }));
    await outbox.settled();
    expect(link.ids()).toEqual([]);
    outbox.dropSessionEnders();
    await outbox.settled();
    expect(revoke.result).toMatchObject({ ok: false, code: "not_done" });
    link.connect(outbox);
    await outbox.settled();
    expect(link.ids()).toEqual([]);
  });

  test("a session ender a reload brought back is still dropped when the session ends", async () => {
    // Queued offline, then the page reloads (or a second tab loads the record): the flag comes back with it.
    const before = new Outbox(idb, () => false);
    void before.submit(revokeMe, { endsSession: true });
    void before.submit(reply());
    await before.settled();

    const link = new FakeLink();
    const after = new Outbox(idb, link.send);
    await after.settled();
    expect(after.pending().map((c) => c.action.case)).toEqual(["revokePasskey", "reply"]);
    after.dropSessionEnders();
    await after.settled();
    // The other command stays, and goes out after the next sign-in; the revoke never does.
    link.connect(after);
    await after.settled();
    expect(link.actions().map((a) => a?.case)).toEqual(["reply"]);
    const again = new Outbox(idb, () => true);
    await again.settled();
    expect(again.pending().map((c) => c.action.case)).toEqual(["reply"]);
  });

  test("a session ending before the reload's commands are loaded still drops their session enders", async () => {
    const before = new Outbox(idb, () => false);
    void before.submit(revokeMe, { endsSession: true });
    void before.submit(reply());
    await before.settled();

    // The start-up check says signed out before IndexedDB answered.
    const after = new Outbox(idb, () => true);
    after.dropSessionEnders();
    await after.settled();
    expect(after.pending().map((c) => c.action.case)).toEqual(["reply"]);
  });

  test("an explicit sign-out before the reload's commands are loaded forgets them too", async () => {
    const before = new Outbox(idb, () => false);
    void before.submit(reply());
    await before.settled();
    const link = new FakeLink();
    const after = new Outbox(idb, link.send);
    after.clear();
    link.connect(after);
    await after.settled();
    expect(after.pending()).toEqual([]);
    expect(link.ids()).toEqual([]);
  });

  test("without IndexedDB the outbox still works in memory", async () => {
    const broken = { open: () => { throw new Error("blocked"); } } as unknown as IDBFactory;
    const reportError = vi.fn();
    const link = new FakeLink();
    const outbox = new Outbox(broken, link.send, { reportError });
    link.connect(outbox);
    const commit = vi.fn();
    const t = track(outbox.submit(reply(), { commit }));
    await outbox.settled();
    expect(link.ids()).toHaveLength(1);
    outbox.onResult(ok(link.ids()[0]));
    await outbox.settled();
    expect(t.result?.ok).toBe(true);
    expect(commit).toHaveBeenCalledTimes(1);
    expect(reportError).toHaveBeenCalled();
  });

  test("a command is sent only after IndexedDB committed it", async () => {
    const db = spied(idb);
    const durableAtSend: boolean[] = [];
    const outbox = new Outbox(db.factory, (cmd) => {
      durableAtSend.push(db.committed.has(cmd.id));
      return true;
    });
    outbox.onOpen();
    void outbox.submit(reply("one"));
    await outbox.settled();
    void outbox.submit(reply("two")); // after the load, with the database open
    await outbox.settled();
    expect(durableAtSend).toEqual([true, true]);
  });

  test("commands a reload brings back go before those submitted during the load", async () => {
    const before = new Outbox(idb, () => false);
    void before.submit(reply("old"));
    await before.settled();

    const link = new FakeLink();
    const after = new Outbox(idb, link.send);
    // Both before the stored commands are read: a submit, then the first open.
    void after.submit(reply("new"));
    link.connect(after);
    await after.settled();
    expect(link.actions()).toMatchObject([reply("old"), reply("new")]);
    expect(link.ids()).toEqual(after.pending().map((c) => c.id));
  });

  test("a refused stale watch keeps a newer watch of the same Ticket", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    link.connect(outbox);
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-1" } });
    void outbox.submit({ case: "unwatchTicket", value: { ticketId: "T-1" } });
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-1" } });
    await outbox.settled();
    const [w1, u, w2] = link.ids();
    outbox.onResult(fail(w1, "not_found"));
    expect(outbox.watched()).toEqual(["T-1"]);
    outbox.onResult(ok(u));
    outbox.onResult(ok(w2));
    expect(outbox.watched()).toEqual(["T-1"]);
  });

  test("a re-watch refused after reconnect is told to onWatchLost", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    const lost = vi.fn();
    outbox.onWatchLost(lost);
    link.connect(outbox);
    void outbox.submit({ case: "watchTicket", value: { ticketId: "T-1" } });
    const direct = track(outbox.submit({ case: "watchTicket", value: { ticketId: "T-9" } }));
    await outbox.settled();
    const [w1, w9] = link.ids();
    outbox.onResult(ok(w1));
    outbox.onResult(fail(w9, "not_found"));
    await outbox.settled();
    // A refusal of the owner's own watch goes to its promise, not to onWatchLost.
    expect(direct.result).toMatchObject({ ok: false, code: "not_found" });
    expect(lost).not.toHaveBeenCalled();

    link.drop();
    link.connect(outbox);
    await outbox.settled();
    const [rewatch] = link.ids();
    outbox.onResult(fail(rewatch, "not_found"));
    expect(lost).toHaveBeenCalledTimes(1);
    expect(lost).toHaveBeenCalledWith("T-1");
    expect(outbox.watched()).toEqual([]);
  });

  test("sign-out runs once per connection however many commands it refused", async () => {
    const link = new FakeLink();
    const outbox = new Outbox(idb, link.send);
    const signOut = vi.fn();
    outbox.setSignOutHandler(signOut);
    link.connect(outbox);
    void outbox.submit(reply("one"));
    void outbox.submit(reply("two"));
    await outbox.settled();
    for (const id of link.ids()) outbox.onResult(fail(id, "unauthenticated"));
    expect(signOut).toHaveBeenCalledTimes(1);
    link.drop();
    link.connect(outbox);
    await outbox.settled();
    expect(link.ids()).toHaveLength(2);
    for (const id of link.ids()) outbox.onResult(fail(id, "unauthenticated"));
    expect(signOut).toHaveBeenCalledTimes(2);
  });
});

describe("newCommandId", () => {
  const CROCKFORD = /^[0-9A-HJKMNP-TV-Z]{26}$/;
  const decodeTime = (id: string) =>
    [...id.slice(0, 10)].reduce((n, c) => n * 32 + "0123456789ABCDEFGHJKMNPQRSTVWXYZ".indexOf(c), 0);

  test("ids are unique and ordered", () => {
    const ids: string[] = [];
    // Ahead of every id made so far in this file, so the first one starts a new millisecond.
    const at = Date.now() + 86400_000;
    for (let i = 0; i < 1000; i++) ids.push(newCommandId(at)); // all in one millisecond
    ids.push(newCommandId(at - 5000)); // the clock steps back
    ids.push(newCommandId(at + 1));
    ids.push(newCommandId());
    for (const id of ids) {
      expect(id).toMatch(CROCKFORD);
      expect(new TextEncoder().encode(id).length).toBeLessThanOrEqual(128);
    }
    expect(new Set(ids).size).toBe(ids.length);
    expect([...ids].sort()).toEqual(ids);
    expect(decodeTime(ids[0])).toBe(at);
    expect(decodeTime(ids[1001])).toBe(at + 1);
  });
});
