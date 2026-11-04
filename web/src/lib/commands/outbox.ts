// The outbox: every Command the owner gives stays here until its CommandResult arrives, so a tap takes effect exactly
// once even when the phone sleeps or the network drops mid-tap. Each open connection (after Welcome) gets every
// command still without a result, in order (a reload's commands first, then by id), by the same id; the server answers a repeat from its stored result
// (kept 24 h) without acting twice, and a repeated result is ignored here. Commands live in IndexedDB `athena`, store
// `outbox`, so they also survive a reload. The rules are docs/dev/dashboard.md's client contract.
//
// Watching a Ticket and reporting visibility belong to the connection: the server keeps no result for them and a new
// connection starts with neither, so they are kept in memory only, and the tracked watches and the last visibility
// are sent again on every open.
import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import {
  CommandResultSchema,
  CommandSchema,
  type Command,
  type CommandResult,
} from "@/gen/athena/v1/commands_pb";
import { Channel, type Frame } from "@/gen/athena/v1/link_pb";
import type { LinkClient } from "@/lib/link/client";
import { useAthena, type AthenaState } from "@/lib/store/state";
import { newCommandId } from "./ids";

/** A Command's action: a generated message or its init shape (the plain object form). */
export type Action = MessageInitShape<typeof CommandSchema>["action"];

export type SubmitOptions = {
  /** Applied to the store at once, before the command is sent. */
  optimistic?: (s: AthenaState) => void;
  /** Undoes optimistic when the command fails; never run on success (the Deltas that follow are the truth). */
  rollback?: (s: AthenaState) => void;
  /** Runs when the command succeeds, once. */
  commit?: (s: AthenaState) => void;
};

export type OutboxOptions = {
  store?: typeof useAthena;
  now?: () => number;
  /**
   * Tells the owner something went wrong outside any one tap (commands dropped as too old). Nothing by default: the
   * app's wiring passes a toast, so this module stays free of UI.
   */
  notify?: (title: string, description: string) => void;
  /** Hears an unexpected error (IndexedDB, a throwing callback); console.error by default. */
  reportError?: (e: unknown) => void;
};

/** The server keeps a result 24 h; a command older than this may be acted on twice if resent, so it is dropped. */
export const EXPIRE_AFTER_MS = 23 * 3600_000;
/** A command goes through at most this many passkey step-ups; a refusal after that fails it. */
const MAX_STEP_UPS = 2;

const DB_NAME = "athena";
const DB_VERSION = 1;
const STORE = "outbox";

/** One stored command, as IndexedDB holds it. */
type Stored = { id: string; bytes: Uint8Array; createdAt: number };

/** The commands that belong to the connection: never stored, re-sent on every open from what is tracked. */
const CONNECTION_ACTIONS = new Set<Command["action"]["case"]>(["watchTicket", "unwatchTicket", "visibility"]);

type Entry = {
  cmd: Command;
  /**
   * The send order: the commands a reload brought back first (negative, in id order), then the submits of this
   * page in the order made, which is their id order too.
   */
  order: number;
  /** Stored, or needing no storage: only a ready command is sent (persist, then send). */
  ready: boolean;
  /** A re-watch the outbox made on reconnect: nobody awaits it, so a refusal is told to onWatchLost. */
  rewatch?: boolean;
  createdAt: number;
  /** Written to IndexedDB (false for the connection's own commands). */
  stored: boolean;
  /** The connection it was last sent on (0: none); a command is sent at most once per connection. */
  sentOn: number;
  /** Waiting for the owner's passkey step-up: not sent until it finishes. */
  held: boolean;
  stepUps: number;
  /** The promise of the submit that made it; a command loaded after a reload has none. */
  resolve?: (r: CommandResult) => void;
  rollback?: (s: AthenaState) => void;
  commit?: (s: AthenaState) => void;
};

export class Outbox {
  private readonly send: (cmd: Command) => boolean | void;
  private readonly store: typeof useAthena;
  private readonly now: () => number;
  private readonly notify: (title: string, description: string) => void;
  private readonly reportError: (e: unknown) => void;

  private readonly entries = new Map<string, Entry>();
  /** The open connection's number, from 1: onOpen counts them. 0 until the first, when nothing is sent. */
  private connection = 0;
  private reauth?: () => Promise<boolean>;
  private signOut?: () => void;
  /** The connection the sign-out handler last ran for: once per connection, however many commands it refused. */
  private signedOutOn = -1;
  private steppingUp = false;
  private lastOrder = 0;
  private readonly watchLost = new Set<(ticketId: string) => void>();

  /** The Tickets watched now, re-watched on every open. */
  private readonly watching = new Set<string>();
  /** The visibility reported last, reported again on every open. */
  private visible?: boolean;

  private readonly db: Promise<IDBDatabase | null>;
  /** IndexedDB writes, one after another in the order they were made, so a delete never overtakes its put. */
  private work: Promise<void> = Promise.resolve();
  private readonly loaded: Promise<void>;

  /** send puts a command on the link; it returns false when the link is not open (nothing was sent). */
  constructor(db: IDBFactory, send: (cmd: Command) => boolean | void, opts: OutboxOptions = {}) {
    this.send = send;
    this.store = opts.store ?? useAthena;
    this.now = opts.now ?? (() => Date.now());
    this.notify = opts.notify ?? (() => {});
    this.reportError = opts.reportError ?? ((e) => console.error("outbox:", e));
    this.db = openDb(db).catch((e: unknown) => {
      // Private browsing or storage turned off: the outbox still works, in memory, until the page goes.
      this.reportError(e);
      return null;
    });
    this.loaded = this.work = this.write(async (db) => {
      const stored = (await all(db)).sort((a, b) => cmpId(a.id, b.id));
      let order = -stored.length;
      for (const rec of stored) {
        order++;
        if (this.entries.has(rec.id)) continue;
        let cmd: Command;
        try {
          cmd = fromBinary(CommandSchema, rec.bytes);
        } catch (e) {
          this.reportError(e);
          await del(db, rec.id);
          continue;
        }
        this.entries.set(rec.id, {
          cmd,
          order: order - 1,
          ready: true,
          createdAt: rec.createdAt,
          stored: true,
          sentOn: 0,
          held: false,
          stepUps: 0,
        });
      }
      this.expire();
    });
  }

  /** Called after a passkey step-up is needed; it resolves true once the owner confirmed (Task 8 registers it). */
  setReauthHandler(fn: (() => Promise<boolean>) | undefined): void {
    this.reauth = fn;
  }

  /**
   * The owner was signed out while commands waited for a step-up: they are not failed and no step-up is counted. They
   * stay, and go out again, by the same id, on the next open (after the next sign-in). Call it before the reauth
   * handler answers false.
   */
  releaseHeld(): void {
    for (const e of this.entries.values()) {
      if (!e.held) continue;
      e.held = false;
      e.sentOn = 0;
    }
  }

  /**
   * Called when a command comes back unauthenticated, once per connection: the commands stay and are sent after the
   * next sign-in.
   */
  setSignOutHandler(fn: (() => void) | undefined): void {
    this.signOut = fn;
  }

  /**
   * Keeps action until its result arrives, sends it if the link is open, and resolves with the result exactly once.
   * Never rejects: a failure resolves `{ ok: false, code }`, after rollback.
   */
  submit(action: Action, opts: SubmitOptions = {}): Promise<CommandResult> {
    return new Promise<CommandResult>((resolve) => this.add(action, opts, resolve));
  }

  /**
   * Fires when a watch the outbox re-sent after a reconnect is refused (the Ticket is gone, or too many are watched):
   * the Ticket has left watched(). Returns the unsubscribe.
   */
  onWatchLost(cb: (ticketId: string) => void): () => void {
    this.watchLost.add(cb);
    return () => this.watchLost.delete(cb);
  }

  /** The link is open (after Welcome): sends every command without a result, in order, then the watches. */
  onOpen(): void {
    const connection = ++this.connection;
    void this.loaded.then(() => {
      if (connection !== this.connection) return; // a newer connection does it
      this.expire();
      this.flush();
      this.resendTracked();
    });
  }

  /** A CommandResult from the link. A result for a command not (or no longer) here is a repeat and is ignored. */
  onResult(r: CommandResult): void {
    const e = this.entries.get(r.id);
    if (!e || e.held) return;
    if (r.ok) {
      this.finish(e, r);
      return;
    }
    switch (r.code) {
      case "reauth_required":
        // Nothing was done; after the step-up the same command goes again.
        if (e.stepUps >= MAX_STEP_UPS) this.finish(e, r);
        else this.stepUp(e);
        return;
      case "unauthenticated":
        // Nothing was done. Kept, and sent on the next open: after the owner signs in again.
        if (this.signedOutOn === this.connection) return;
        this.signedOutOn = this.connection;
        this.guard(() => this.signOut?.());
        return;
      default:
        this.finish(e, r);
    }
  }

  /** The commands without a result, in send order. */
  pending(): Command[] {
    return this.sorted().map((e) => e.cmd);
  }

  /** The Tickets watched now: re-watched after every reconnect. */
  watched(): string[] {
    return [...this.watching];
  }

  /**
   * Forgets every command: only for an explicit sign-out, when nothing may be sent for this device again. Each
   * waiting submit resolves unauthenticated, after its rollback.
   */
  clear(): void {
    for (const e of this.sorted()) this.finish(e, failure(e.cmd.id, "unauthenticated", "signed out"));
    this.watching.clear();
    this.visible = undefined;
    this.work = this.write((db) => clearAll(db));
  }

  /**
   * Resolves once the IndexedDB work queued so far is done and the sends waiting on it have run (a macrotask later,
   * so every microtask after the writes has too). For tests and diagnostics; the outbox never needs it.
   */
  async settled(): Promise<void> {
    for (;;) {
      const w = this.work;
      await w;
      await new Promise((r) => setTimeout(r, 0));
      if (w === this.work) return;
    }
  }

  // Internals.

  private add(action: Action, opts: SubmitOptions, resolve?: (r: CommandResult) => void, rewatch = false): void {
    const cmd = create(CommandSchema, { id: newCommandId(), action });
    const stored = !CONNECTION_ACTIONS.has(cmd.action.case);
    const e: Entry = {
      cmd,
      order: ++this.lastOrder,
      ready: false,
      createdAt: this.now(),
      stored,
      sentOn: 0,
      held: false,
      stepUps: 0,
      resolve,
      rollback: opts.rollback,
      commit: opts.commit,
      rewatch,
    };
    this.entries.set(cmd.id, e);
    this.track(cmd.action);
    if (opts.optimistic) this.guard(() => opts.optimistic!(this.store.getState()));
    // Persist, then send; a command of the connection waits only for the reload's commands, to go after them.
    let persisted = this.loaded;
    if (stored) {
      const rec: Stored = { id: cmd.id, bytes: toBinary(CommandSchema, cmd), createdAt: e.createdAt };
      persisted = this.work = this.write((db) => put(db, rec));
    }
    void persisted.then(() => {
      e.ready = true;
      this.flush();
    });
  }

  /**
   * Sends, in order, every command not yet sent on this connection. It stops at the first one not yet stored, so
   * nothing overtakes it; one waiting for a step-up is skipped.
   */
  private flush(): void {
    for (const e of this.sorted()) {
      if (!e.ready) return;
      this.trySend(e);
    }
  }

  private trySend(e: Entry): void {
    if (e.held || e.sentOn === this.connection || this.entries.get(e.cmd.id) !== e) return;
    let sent: boolean | void = false;
    try {
      sent = this.send(e.cmd);
    } catch (err) {
      this.reportError(err);
      sent = false;
    }
    // Not open: the next onOpen sends it.
    if (sent !== false) e.sentOn = this.connection;
  }

  private finish(e: Entry, r: CommandResult): void {
    if (this.entries.get(e.cmd.id) !== e) return;
    this.entries.delete(e.cmd.id);
    if (e.stored) this.work = this.write((db) => del(db, e.cmd.id));
    if (r.ok) {
      if (e.commit) this.guard(() => e.commit!(this.store.getState()));
    } else {
      if (e.cmd.action.case === "watchTicket") this.watchRefused(e, e.cmd.action.value.ticketId);
      if (e.rollback) this.guard(() => e.rollback!(this.store.getState()));
    }
    e.resolve?.(r);
  }

  /** Holds e for the owner's passkey, asking once for every command that needs it meanwhile. */
  private stepUp(e: Entry): void {
    e.held = true;
    if (this.steppingUp) return;
    this.steppingUp = true;
    void (async () => {
      let ok = false;
      try {
        ok = this.reauth ? await this.reauth() : false;
      } catch (err) {
        this.reportError(err);
      }
      this.steppingUp = false;
      for (const h of this.sorted()) {
        if (!h.held) continue;
        if (ok) {
          h.held = false;
          h.stepUps++;
          h.sentOn = 0; // not answered: send it again, on this connection too
        } else {
          this.finish(h, failure(h.cmd.id, "reauth_required", "step-up refused"));
        }
      }
      if (ok) this.flush();
    })();
  }

  /** Drops the commands too old to resend safely, telling the owner once. */
  private expire(): void {
    const cutoff = this.now() - EXPIRE_AFTER_MS;
    let dropped = 0;
    for (const e of this.sorted()) {
      if (e.createdAt > cutoff) continue;
      dropped++;
      this.finish(e, failure(e.cmd.id, "expired", "waited too long to send"));
    }
    if (dropped) {
      const what = dropped === 1 ? "An action" : `${dropped} actions`;
      this.guard(() => this.notify("Not sent", `${what} waited offline for over 23 hours. Try again.`));
    }
  }

  /**
   * A watch the server refused (an unknown Ticket, too many) is not watched, unless a later watch of the same Ticket
   * is pending: the owner's latest intent wins. A refused re-watch is told to onWatchLost.
   */
  private watchRefused(e: Entry, ticketId: string): void {
    for (const later of this.entries.values()) {
      const a = later.cmd.action;
      if (later.order > e.order && a.case === "watchTicket" && a.value.ticketId === ticketId) return;
    }
    if (!this.watching.delete(ticketId) || !e.rewatch) return;
    for (const cb of [...this.watchLost]) this.guard(() => cb(ticketId));
  }

  /** Keeps what is watched and the last visibility, for the next connection. */
  private track(a: Command["action"]): void {
    switch (a.case) {
      case "watchTicket":
        this.watching.add(a.value.ticketId);
        break;
      case "unwatchTicket":
        this.watching.delete(a.value.ticketId);
        break;
      case "visibility":
        this.visible = a.value.visible;
        break;
    }
  }

  /** A new connection watches nothing and knows no visibility: sends what is tracked, unless one is still pending. */
  private resendTracked(): void {
    const pendingWatch = new Set<string>();
    let pendingVisibility = false;
    for (const e of this.entries.values()) {
      const a = e.cmd.action;
      if (a.case === "watchTicket" || a.case === "unwatchTicket") pendingWatch.add(a.value.ticketId);
      if (a.case === "visibility") pendingVisibility = true;
    }
    for (const id of this.watching) {
      if (!pendingWatch.has(id)) this.add({ case: "watchTicket", value: { ticketId: id } }, {}, undefined, true);
    }
    if (this.visible !== undefined && !pendingVisibility) {
      this.add({ case: "visibility", value: { visible: this.visible } }, {});
    }
  }

  /** The commands in send order. */
  private sorted(): Entry[] {
    return [...this.entries.values()].sort((a, b) => a.order - b.order);
  }

  /** Queues an IndexedDB step after the ones before it; without a database it does nothing. */
  private write(step: (db: IDBDatabase) => Promise<void>): Promise<void> {
    return this.work.then(async () => {
      const db = await this.db;
      if (!db) return;
      try {
        await step(db);
      } catch (e) {
        this.reportError(e);
      }
    });
  }

  private guard(fn: () => void): void {
    try {
      fn();
    } catch (e) {
      this.reportError(e);
    }
  }
}

/** The sender for a LinkClient: Commands go on CONTROL as durable frames. */
export function linkSender(client: LinkClient): (cmd: Command) => boolean {
  return (cmd) => client.send(Channel.CONTROL, true, { case: "command", value: cmd });
}

/** Feeds outbox from client: every open resends what has no result, and CommandResults go in. Returns the unsubscribe. */
export function connectOutbox(client: LinkClient, outbox: Outbox): () => void {
  const offStatus = client.onStatus((s) => {
    if (s === "open") outbox.onOpen();
  });
  const offFrame = client.onFrame((f: Frame) => {
    if (f.body.case === "commandResult") outbox.onResult(f.body.value);
  });
  if (client.status === "open") outbox.onOpen();
  return () => {
    offStatus();
    offFrame();
  };
}

function cmpId(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

function failure(id: string, code: string, message: string): CommandResult {
  return create(CommandResultSchema, { id, ok: false, code, message });
}

// Raw IndexedDB, promised.

function openDb(factory: IDBFactory): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = factory.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(STORE)) req.result.createObjectStore(STORE, { keyPath: "id" });
    };
    req.onsuccess = () => {
      const db = req.result;
      // Another tab upgrading the database: let it, this one carries on in memory.
      db.onversionchange = () => db.close();
      resolve(db);
    };
    req.onerror = () => reject(req.error);
    req.onblocked = () => reject(new Error("outbox: the database is blocked by another tab"));
  });
}

function run(db: IDBDatabase, mode: IDBTransactionMode, step: (s: IDBObjectStore) => IDBRequest): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, mode);
    const req = step(tx.objectStore(STORE));
    tx.oncomplete = () => resolve(req.result);
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new Error("outbox: transaction aborted"));
  });
}

async function all(db: IDBDatabase): Promise<Stored[]> {
  return (await run(db, "readonly", (s) => s.getAll())) as Stored[];
}

async function put(db: IDBDatabase, rec: Stored): Promise<void> {
  await run(db, "readwrite", (s) => s.put(rec));
}

async function del(db: IDBDatabase, id: string): Promise<void> {
  await run(db, "readwrite", (s) => s.delete(id));
}

async function clearAll(db: IDBDatabase): Promise<void> {
  await run(db, "readwrite", (s) => s.clear());
}
