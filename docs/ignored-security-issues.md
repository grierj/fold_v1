# Intentionally ignored security issues

Record security review findings here when likelihood multiplied by severity is 4 or less.
For each finding, include its date, affected code or behavior, evidence, likelihood and
severity scores with their reasoning, the product, and the reviewed revision. Do not include
credentials, personal data, or exploit instructions.

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
