# Intentionally ignored security issues

Record security review findings here when likelihood multiplied by severity is 4 or less.
For each finding, include its date, affected code or behavior, evidence, likelihood and
severity scores with their reasoning, the product, and the reviewed revision. Do not include
credentials, personal data, or exploit instructions.

## Request body retained by a non-cooperating transport

- Date: 2026-11-21. Reviewed revision: `c2796148513f9f19d23bd44f764633a2554c4b58`
  with the uncommitted B1 in-flight disposal changes. The reviewed seven-file manifest has
  SHA-256 `9ed577736cb5642a5c4013082c77ff7775f3ac142495977c379ea2e8d0e5d5b5`.
- A transport that ignores cancellation and never settles can retain a serialized date-edit
  request in its pending frames after the attempt store is disposed. The retained string can
  contain private text. The review found no cross-user disclosure or continuing authority.
- Likelihood: **1/10**. The browser fetch path receives an abort signal; this case requires a
  transport that ignores it indefinitely.
- Severity: **4/10**. Private text may remain in the same browser process, but the review found
  no publication or replay path. Product: **4**. The issue is intentionally not fixed.
- Evidence: `web/src/lib/dateWrites.tsx`, `web/src/api/events.ts`, and `web/src/api/core.ts`.
