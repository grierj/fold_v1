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

## A redundant group status is accepted from a leader

- Date: 2026-11-22. Reviewed revision: `b2303a962528189bd66cea4fd0ea277d4336f6da`.
- A leader who sends the status their group already has, along with other edits, gets 200.
  Before DEC-160 any status a leader could not set answered 403, the current one included.
  The edit already succeeded with the status left out, so the request gains nothing and reads
  nothing new.
- Likelihood: **1/10**. Only a hand-made request sends it; the web client sends no group status.
- Severity: **1/10**. No capability or disclosure: the caller already manages the group.
  Product: **1**. The issue is intentionally not fixed.
- Evidence: `policy.DecideGroupStatus` in `internal/policy/policy.go` and `apiUpdateGroup` in
  `api_groups.go`.
