import { fromBinary, toBinary } from "@bufbuild/protobuf";
import { FrameSchema, type Frame } from "@/gen/athena/v1/link_pb";

/** The wire form of one Frame: one binary WebSocket message. */
export function encodeFrame(f: Frame): Uint8Array {
  return toBinary(FrameSchema, f);
}

/**
 * Reads one Frame. Unknown fields are skipped, so a newer server's additions do not break this client; only the
 * server is strict. Malformed bytes throw.
 */
export function decodeFrame(b: Uint8Array): Frame {
  return fromBinary(FrameSchema, b, { readUnknownFields: false });
}
