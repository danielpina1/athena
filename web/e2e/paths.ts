// Where the e2e harness finds the repository and the binaries global-setup builds, and the enroll link's shape.
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
/**
 * The e2e builds of athena, fakeclaude and sessionctl (git-ignored). Never bin/athena: the owner's real server must
 * not pick up a work-in-progress build (ruling R39).
 */
export const BIN = join(ROOT, "bin", "e2e");
/** The link `athena web enroll` prints: http with TLS off (the shared server), https with Athena's own CA. */
export const ENROLL_LINK = /https?:\/\/localhost:\d+\/enroll#\S+/;
