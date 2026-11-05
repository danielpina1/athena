// A test double for the server end of the link: an in-memory WebSocket that speaks real encoded Frames. It encodes
// and decodes with @bufbuild/protobuf directly, not through frame.ts, so the client's encoding is checked by an
// independent implementation. Test helper only; nothing in the app imports it.
import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { Channel, FrameSchema, type Frame } from "@/gen/athena/v1/link_pb";

type BodyCase = NonNullable<Frame["body"]["case"]>;
type BodyOf<K extends BodyCase> = Extract<Frame["body"], { case: K }>["value"];
type CloseEventLike = { code: number; reason: string; wasClean: boolean };
type MessageEventLike = { data: unknown };

/** One fake socket: the client holds it as its WebSocket, the test drives it through FakeServer. */
export class FakeSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  readonly url: string;
  readonly protocols: string[];
  binaryType = "blob";
  readyState = FakeSocket.CONNECTING;
  protocol = "";
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: MessageEventLike) => void) | null = null;
  onclose: ((ev: CloseEventLike) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;

  /** The frames the client sent on this socket, decoded. */
  readonly received: Frame[] = [];
  /** Rule breaks a real server would refuse: a first frame that is not a Hello, a durable seq out of order. */
  readonly violations: string[] = [];
  /** The close the client asked for, if it closed this socket. */
  closedByClient?: { code?: number; reason?: string };
  private serverSeq = 0n;
  private clientSeq = 0n;

  constructor(url: string, protocols?: string | string[]) {
    this.url = url;
    this.protocols = protocols === undefined ? [] : Array.isArray(protocols) ? protocols : [protocols];
  }

  // The client's side of the socket.

  send(data: unknown): void {
    if (this.readyState !== FakeSocket.OPEN) throw new Error("send on a socket that is not open");
    if (!(data instanceof Uint8Array)) {
      this.violations.push(`sent ${typeof data}, not binary`);
      return;
    }
    const f = fromBinary(FrameSchema, data);
    if (this.received.length === 0 && f.body.case !== "hello") this.violations.push(`first frame is ${f.body.case}`);
    if (f.seq !== 0n) {
      if (f.seq !== this.clientSeq + 1n) this.violations.push(`durable seq ${f.seq} after ${this.clientSeq}`);
      this.clientSeq = f.seq;
    }
    this.received.push(f);
  }

  close(code?: number, reason?: string): void {
    if (this.readyState === FakeSocket.CLOSED) return;
    this.closedByClient = { code, reason };
    this.readyState = FakeSocket.CLOSED;
    this.onclose?.({ code: code ?? 1005, reason: reason ?? "", wasClean: true });
  }

  // The server's side, for tests.

  /** Completes the handshake: the server accepted the upgrade with athena.v1. */
  accept(): void {
    this.protocol = this.protocols.includes("athena.v1") ? "athena.v1" : "";
    this.readyState = FakeSocket.OPEN;
    this.onopen?.({});
  }

  /** Delivers one encoded Frame to the client. Durable frames are numbered here, from 1, like the server's Session. */
  push(init: MessageInitShape<typeof FrameSchema>, opts: { durable?: boolean; ack?: bigint } = {}): Frame {
    const f = create(FrameSchema, { channel: Channel.CONTROL, ...init });
    if (opts.durable) f.seq = ++this.serverSeq;
    if (opts.ack !== undefined) f.ack = opts.ack;
    this.deliver(toBinary(FrameSchema, f));
    return f;
  }

  /** Delivers raw bytes as a binary message (to test decoding, unknown fields and garbage). */
  deliver(bytes: Uint8Array): void {
    if (this.readyState !== FakeSocket.OPEN) throw new Error("deliver on a socket that is not open");
    const copy = bytes.slice();
    this.onmessage?.({ data: this.binaryType === "arraybuffer" ? copy.buffer : new Blob([copy]) });
  }

  /** The server closes the connection with code. */
  serverClose(code: number, reason = ""): void {
    if (this.readyState === FakeSocket.CLOSED) return;
    this.readyState = FakeSocket.CLOSED;
    this.onclose?.({ code, reason, wasClean: code !== 1006 });
  }

  /** The frames the client sent of one body kind. */
  sent<K extends BodyCase>(kind: K): BodyOf<K>[] {
    const out: BodyOf<K>[] = [];
    for (const f of this.received) if (f.body.case === kind) out.push(f.body.value as BodyOf<K>);
    return out;
  }
}

/** The server: it records every socket the client opens; the latest is `socket`. */
export class FakeServer {
  readonly sockets: FakeSocket[] = [];
  /** Pass as LinkClient's WebSocketImpl. */
  readonly WebSocket: typeof WebSocket;

  constructor() {
    const sockets = this.sockets;
    class BoundSocket extends FakeSocket {
      constructor(url: string, protocols?: string | string[]) {
        super(url, protocols);
        sockets.push(this);
      }
    }
    this.WebSocket = BoundSocket as unknown as typeof WebSocket;
  }

  get socket(): FakeSocket {
    const s = this.sockets.at(-1);
    if (!s) throw new Error("the client opened no socket");
    return s;
  }

  /** Accepts the latest socket and answers its Hello with a Welcome. */
  welcome(epoch = "e1", snapshotFollows = true): FakeSocket {
    const s = this.socket;
    if (s.readyState === FakeSocket.CONNECTING) s.accept();
    s.push({ body: { case: "welcome", value: { epoch, serverVersion: "test", heartbeatMs: 15000, snapshotFollows } } });
    return s;
  }
}
