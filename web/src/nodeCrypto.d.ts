// The digest tests compare against Node's SHA-256, the seeded suites record
// the seeds they ran through the filesystem, and the date-edit retention test
// asks for a real garbage collection. The web project carries no @types/node,
// so this declares the little they use: two crypto functions, two fs functions,
// the env record vitest provides, and the v8 flag and vm context that reach gc().
declare module "node:crypto" {
  interface Hash {
    update(data: Uint8Array | string): Hash;
    digest(): Uint8Array;
    digest(encoding: "hex"): string;
  }

  export function createHash(algorithm: "sha256"): Hash;
  export function randomBytes(size: number): Uint8Array;
}

declare module "node:fs" {
  export function appendFileSync(path: string, data: string): void;
  export function readFileSync(path: string | URL, encoding: "utf8"): string;
}

declare module "node:process" {
  export const env: Record<string, string | undefined>;
}

declare module "node:v8" {
  export function setFlagsFromString(flags: string): void;
}

declare module "node:vm" {
  // Only once setFlagsFromString("--expose-gc") has run does a new context have gc.
  export function runInNewContext(code: "gc"): () => void;
}
