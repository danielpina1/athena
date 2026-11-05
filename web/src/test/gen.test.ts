import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { expect, test } from "vitest";
import { FrameSchema, HelloSchema } from "@/gen/athena/v1/link_pb.js";

// Lives outside web/src/gen: buf generate runs with clean: true and would delete a test placed there.
test("gen round-trips a Frame", () => {
  const hello = create(HelloSchema, { protocol: 1, clientId: "dev-1", epoch: "e1", cursor: 7n });
  const frame = create(FrameSchema, { seq: 3n, body: { case: "hello", value: hello } });
  const back = fromBinary(FrameSchema, toBinary(FrameSchema, frame));
  expect(back.seq).toBe(3n);
  expect(back.body.case).toBe("hello");
  if (back.body.case !== "hello") throw new Error("not a hello");
  expect(back.body.value.protocol).toBe(1);
  expect(back.body.value.clientId).toBe("dev-1");
  expect(back.body.value.epoch).toBe("e1");
  expect(back.body.value.cursor).toBe(7n);
});
