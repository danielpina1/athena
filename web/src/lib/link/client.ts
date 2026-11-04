// The browser end of the link: one WebSocket (subprotocol athena.v1, one binary Frame per message) kept open with
// reconnects. It numbers and acks durable frames, answers Pings, notices a dead connection and backs off between
// attempts. It knows nothing of React or of the state: connectStore (lib/store/state.ts) feeds the store from it.
// The wire rules are docs/dev/dashboard.md's client contract.
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Channel, FrameSchema, type Frame } from "@/gen/athena/v1/link_pb";
import { decodeFrame, encodeFrame } from "./frame";

export type LinkStatus = "connecting" | "open" | "offline" | "signedOut";

/** A frame body to send: a generated message or its init shape. */
export type FrameBody = MessageInitShape<typeof FrameSchema>["body"];

export type LinkClientOptions = {
  url: string;
  clientId: string;
  WebSocketImpl?: typeof WebSocket;
  now?: () => number;
};

export const SUBPROTOCOL = "athena.v1";
export const PROTOCOL = 1;

/** A connection with no frame for this long is dead: the server pings every 15 s. */
export const DEAD_AFTER_MS = 45_000;
/** A received durable frame is acked on its own after this long when nothing else carried the ack. */
const ACK_DELAY_MS = 200;
/** Reconnect backoff: 0.5 s doubling to 15 s, ±20 % jitter, back to 0.5 s after 10 s connected. */
const BACKOFF_MIN_MS = 500;
const BACKOFF_MAX_MS = 15_000;
const BACKOFF_JITTER = 0.2;
const STABLE_AFTER_MS = 10_000;
/** The attempt whose backoff is the maximum. */
const MAX_BACKOFF_ATTEMPT = Math.ceil(Math.log2(BACKOFF_MAX_MS / BACKOFF_MIN_MS));

const CLOSE_NORMAL = 1000;
/** Ours: the server went silent. The server's own 4000 means it found us silent. */
const CLOSE_DEAD = 4000;
/** The peer broke the framing or sequencing rules. */
const CLOSE_PROTOCOL = 4001;
/** The login behind the connection ended. */
const CLOSE_SIGNED_OUT = 4002;

const OPEN = 1; // WebSocket.OPEN, read without touching the global
const MAX_CREDIT = 2 ** 32 - 1;

type Timer = ReturnType<typeof setTimeout>;

export class LinkClient {
  private readonly url: string;
  private readonly clientId: string;
  private readonly WS: typeof WebSocket;
  private readonly now: () => number;

  private _status: LinkStatus = "offline";
  private running = false;
  private listening = false;
  private readonly statusCbs = new Set<(s: LinkStatus) => void>();
  private readonly frameCbs = new Set<(f: Frame) => void>();

  // What the next Hello resumes from.
  private epoch = "";
  private cursor = 0n;

  // The current connection. Each socket is its own link session: frames are numbered from 1 in each direction.
  private ws: WebSocket | null = null;
  private sendSeq = 0n;
  private recvSeq = 0n;
  private ackDue = false;
  private welcomed = false;
  private openedAt = 0;
  private lastRecv = 0;

  private attempt = 0;
  private reconnectTimer: Timer | null = null;
  private deadTimer: Timer | null = null;
  private ackTimer: Timer | null = null;

  constructor(opts: LinkClientOptions) {
    this.url = opts.url;
    this.clientId = opts.clientId;
    this.WS = opts.WebSocketImpl ?? WebSocket;
    this.now = opts.now ?? (() => Date.now());
  }

  get status(): LinkStatus {
    return this._status;
  }

  /** Connects and keeps connecting until stop() or a signed-out close. After a sign-out, start() connects afresh. */
  start(): void {
    if (this.running && this._status !== "signedOut") return;
    this.running = true;
    this.attempt = 0;
    this.listen(true);
    this.connect();
  }

  /** Closes the connection and stops reconnecting. A signed-out client stays signedOut. */
  stop(): void {
    this.running = false;
    this.listen(false);
    this.clearReconnect();
    const ws = this.detach();
    closeQuietly(ws, CLOSE_NORMAL, "stopped");
    if (this._status !== "signedOut") this.setStatus("offline");
  }

  onStatus(cb: (s: LinkStatus) => void): () => void {
    this.statusCbs.add(cb);
    return () => this.statusCbs.delete(cb);
  }

  /** Every frame the server sends except Ping and Pong, in order, each once (a repeated durable frame is dropped). */
  onFrame(cb: (f: Frame) => void): () => void {
    this.frameCbs.add(cb);
    return () => this.frameCbs.delete(cb);
  }

  /**
   * Sends one frame on channel; a durable one takes the connection's next seq. Only an open link (after Welcome)
   * sends: anything else returns false and sends nothing, since nothing carries over to the next connection.
   */
  send(channel: Channel, durable: boolean, body: FrameBody): boolean {
    if (this._status !== "open" || !this.ws) return false;
    const f = create(FrameSchema, { channel, body });
    if (durable) f.seq = ++this.sendSeq;
    return this.write(this.ws, f);
  }

  /** The epoch of the last Welcome whose state was applied, and the highest Delta seq applied: the next Hello's. */
  setResume(epoch: string, cursor: bigint): void {
    this.epoch = epoch;
    this.cursor = cursor;
  }

  /**
   * Grants the server items more STREAM credit on this connection. Each connection starts with an implicit 256, and
   * grants add to it; credit does not carry over, so an offline grant sends nothing.
   */
  grantStream(items: number): boolean {
    const n = Math.min(Math.floor(items), MAX_CREDIT);
    if (!(n > 0)) return false;
    return this.send(Channel.CONTROL, false, { case: "credit", value: { channel: Channel.STREAM, items: n } });
  }

  // The connection.

  private connect(): void {
    this.clearReconnect();
    let ws: WebSocket;
    try {
      ws = new this.WS(this.url, SUBPROTOCOL);
    } catch {
      this.retry(false);
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    this.sendSeq = 0n;
    this.recvSeq = 0n;
    this.ackDue = false;
    this.welcomed = false;
    this.openedAt = 0;
    // A connect that hangs counts as silence too.
    this.lastRecv = this.now();
    this.armDead(DEAD_AFTER_MS);
    ws.onopen = () => {
      if (this.ws !== ws) return;
      // Not durable: the client's durable frames are numbered from 1 after it.
      this.write(ws, create(FrameSchema, {
        channel: Channel.CONTROL,
        body: { case: "hello", value: { protocol: PROTOCOL, clientId: this.clientId, epoch: this.epoch, cursor: this.cursor } },
      }));
    };
    ws.onmessage = (ev: MessageEvent) => {
      if (this.ws === ws) this.receive(ws, ev.data);
    };
    ws.onclose = (ev: CloseEvent) => {
      if (this.ws === ws) this.closed(ev.code);
    };
    ws.onerror = () => {}; // a close follows
    this.setStatus("connecting");
  }

  private receive(ws: WebSocket, data: unknown): void {
    let f: Frame;
    try {
      if (!(data instanceof ArrayBuffer)) throw new Error("not a binary frame");
      f = decodeFrame(new Uint8Array(data));
    } catch {
      this.drop(CLOSE_PROTOCOL, "undecodable frame", false);
      return;
    }
    this.lastRecv = this.now();
    if (f.seq !== 0n) {
      if (f.seq <= this.recvSeq) {
        // The server missed our ack: say it again, and do not deliver the frame twice.
        this.ackSoon(ws);
        return;
      }
      if (f.seq !== this.recvSeq + 1n) {
        this.drop(CLOSE_PROTOCOL, "sequence gap", false);
        return;
      }
      this.recvSeq = f.seq;
      this.ackSoon(ws);
    }
    switch (f.body.case) {
      case "ping":
        // A Ping at 0 is a standalone ack: nothing to answer.
        if (f.body.value.atUnixMs !== 0n) {
          this.write(ws, create(FrameSchema, { channel: Channel.CONTROL, body: { case: "pong", value: { atUnixMs: f.body.value.atUnixMs } } }));
        }
        return;
      case "pong":
        return;
      case "welcome":
        this.emitFrame(f);
        if (this.ws !== ws || this.welcomed) return;
        this.welcomed = true;
        this.openedAt = this.now();
        this.setStatus("open");
        return;
      default:
        this.emitFrame(f);
    }
  }

  /** Writes f with the cumulative ack; any frame carries the ack, so no standalone one is due after it. */
  private write(ws: WebSocket, f: Frame): boolean {
    if (ws.readyState !== OPEN) return false;
    f.ack = this.recvSeq;
    try {
      // protobuf-es allocates a plain ArrayBuffer; the DOM type wants it said.
      ws.send(encodeFrame(f) as Uint8Array<ArrayBuffer>);
    } catch {
      return false;
    }
    this.ackDue = false;
    return true;
  }

  private ackSoon(ws: WebSocket): void {
    this.ackDue = true;
    if (this.ackTimer) return;
    this.ackTimer = setTimeout(() => {
      this.ackTimer = null;
      if (this.ws === ws && this.ackDue) {
        this.write(ws, create(FrameSchema, { channel: Channel.CONTROL, body: { case: "ping", value: { atUnixMs: 0n } } }));
      }
    }, ACK_DELAY_MS);
  }

  private armDead(ms: number): void {
    this.deadTimer = setTimeout(() => {
      this.deadTimer = null;
      const left = DEAD_AFTER_MS - (this.now() - this.lastRecv);
      if (left <= 0) this.drop(CLOSE_DEAD, "peer silent", false);
      else this.armDead(left);
    }, ms);
  }

  /** The server or the network closed the connection. */
  private closed(code: number): void {
    this.detach();
    if (code === CLOSE_SIGNED_OUT) {
      this.clearReconnect();
      this.setStatus("signedOut");
      return;
    }
    // A protocol close means this client broke a rule: retrying fast would only repeat it.
    if (code === CLOSE_PROTOCOL) this.attempt = Math.max(this.attempt, MAX_BACKOFF_ATTEMPT);
    this.retry(false);
  }

  /** We end the connection (dead, or the server broke a rule) and reconnect. */
  private drop(code: number, reason: string, immediately: boolean): void {
    closeQuietly(this.detach(), code, reason);
    this.retry(immediately);
  }

  /** Forgets the current socket, so its late events are ignored, and returns it. */
  private detach(): WebSocket | null {
    const ws = this.ws;
    this.ws = null;
    if (ws) {
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
    }
    if (this.deadTimer) clearTimeout(this.deadTimer);
    if (this.ackTimer) clearTimeout(this.ackTimer);
    this.deadTimer = this.ackTimer = null;
    this.welcomed = false;
    return ws;
  }

  private retry(immediately: boolean): void {
    if (!this.running) {
      this.setStatus("offline");
      return;
    }
    if (this.openedAt && this.now() - this.openedAt >= STABLE_AFTER_MS) this.attempt = 0;
    this.openedAt = 0;
    if (immediately) {
      this.connect();
      return;
    }
    this.setStatus("offline");
    const base = Math.min(BACKOFF_MIN_MS * 2 ** this.attempt, BACKOFF_MAX_MS);
    const delay = Math.round(base * (1 + (Math.random() * 2 - 1) * BACKOFF_JITTER));
    this.attempt = Math.min(this.attempt + 1, MAX_BACKOFF_ATTEMPT + 1);
    this.clearReconnect();
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  private clearReconnect(): void {
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
  }

  /** The network came back or the page became visible: reconnect now rather than at the end of the backoff. */
  private wake = (): void => {
    if (!this.running || this._status === "signedOut") return;
    if (this.ws) {
      // Timers sleep in a hidden tab; a connection silent past the limit is replaced at once.
      if (this.now() - this.lastRecv >= DEAD_AFTER_MS) this.drop(CLOSE_DEAD, "peer silent", true);
      return;
    }
    this.connect();
  };

  private onVisibility = (): void => {
    if (document.visibilityState === "visible") this.wake();
  };

  private listen(on: boolean): void {
    if (on === this.listening) return;
    this.listening = on;
    if (typeof window !== "undefined") {
      if (on) window.addEventListener("online", this.wake);
      else window.removeEventListener("online", this.wake);
    }
    if (typeof document !== "undefined") {
      if (on) document.addEventListener("visibilitychange", this.onVisibility);
      else document.removeEventListener("visibilitychange", this.onVisibility);
    }
  }

  private setStatus(s: LinkStatus): void {
    if (s === this._status) return;
    this._status = s;
    for (const cb of [...this.statusCbs]) cb(s);
  }

  private emitFrame(f: Frame): void {
    for (const cb of [...this.frameCbs]) cb(f);
  }
}

function closeQuietly(ws: WebSocket | null, code: number, reason: string): void {
  try {
    ws?.close(code, reason);
  } catch {
    // already closing, or a code the implementation refuses: the socket is forgotten either way
  }
}
