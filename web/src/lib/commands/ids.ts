// Command ids: ULIDs (26 Crockford base32 characters: 48 bits of milliseconds, then 80 random bits), so ids sort in
// the order they were made. Within one tab they are strictly increasing even when several come in one millisecond or
// the clock steps back: the random part of the last id is incremented instead. 26 ASCII bytes is inside the server's
// 1 to 128 byte limit.

const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
const TIME_LEN = 10;
const RANDOM_LEN = 16;
const MAX_TIME = 2 ** 48 - 1;

let lastTime = -1;
/** The last id's random part, one base32 digit (0 to 31) per character. */
let lastRandom: number[] = [];

function randomDigits(): number[] {
  const bytes = new Uint8Array(RANDOM_LEN);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b & 31);
}

/** Adds one to digits in place; false when it overflowed (every digit was 31). */
function increment(digits: number[]): boolean {
  for (let i = digits.length - 1; i >= 0; i--) {
    if (digits[i] < 31) {
      digits[i]++;
      return true;
    }
    digits[i] = 0;
  }
  return false;
}

function encodeTime(ms: number): string {
  let out = "";
  for (let i = 0; i < TIME_LEN; i++) {
    out = ALPHABET[ms % 32] + out;
    ms = Math.floor(ms / 32);
  }
  return out;
}

/** A new command id, greater than every id this tab made before. */
export function newCommandId(now: number = Date.now()): string {
  let time = Math.min(Math.max(Math.floor(now), 0), MAX_TIME);
  if (time > lastTime) {
    lastRandom = randomDigits();
  } else {
    time = lastTime;
    // 2^80 ids in one millisecond cannot happen; borrowing the next millisecond keeps the order regardless.
    if (!increment(lastRandom)) {
      time = Math.min(time + 1, MAX_TIME);
      lastRandom = randomDigits();
    }
  }
  lastTime = time;
  return encodeTime(time) + lastRandom.map((d) => ALPHABET[d]).join("");
}
