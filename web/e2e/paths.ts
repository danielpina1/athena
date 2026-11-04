// Where the e2e harness finds the repository and the binaries global-setup builds, and the enroll link's shape.
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
export const BIN = join(ROOT, "bin");
/** The link `athena web enroll` prints. */
export const ENROLL_LINK = /http:\/\/localhost:\d+\/enroll#\S+/;
