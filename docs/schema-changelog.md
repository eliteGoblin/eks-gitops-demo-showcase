# GraphQL schema changelog

The schema (`internal/adapter/graphql/schema/*.graphqls`) is the contract with
the web, portal and mobile clients. It is versionless: it only grows, and
nothing is removed without a deprecation period.

## How the schema evolves

1. **Additive by default.** New types, fields, optional arguments and input
   fields are safe. A new union member or enum value is *dangerous* (an old
   client may not handle it), so it is announced here.
2. **Deprecate, measure, then remove.** A field that should go away gets
   `@deprecated(reason: "Use X; removal after <date>")` and a replacement in the
   same release. The `graphql.deprecated_field_usage` metric
   (`graphql_deprecated_field_usage_total{field="Type.field"}` in Prometheus)
   counts the operations that still select it. The field is removed only after
   the date **and** after that counter has stayed at zero for the agreed window
   (30 days), in a pull request labelled `breaking-change-approved`.
3. **The CI gate (advisory).** The `graphql-schema` workflow diffs every pull
   request's schema against the base branch with graphql-inspector. A breaking
   change (removed or renamed field, changed type, new required argument)
   turns the check red unless the pull request carries the label
   `breaking-change-approved`; dangerous and safe changes are listed in the job
   summary. **The check does not block merging:** on GitHub's free plan a
   private repository cannot require status checks (the branch-protection and
   rulesets APIs answer 403), so a red check is a signal reviewers must not
   merge past, not a lock. Further caveats: the label is not tied to a commit
   (it stays approved after later pushes), anyone with triage permission can
   add it, and the check runs the pull request's own copy of the script. When
   the repository is public or on a paid plan: make `graphql-schema` a
   required check and add CODEOWNERS for `.github/` and `scripts/ci/`.

## Scheduled removals

How to read the usage counter: every API pod starts a series for each
deprecated field at `0` when it starts, so a field nobody uses reads as an
increase of `0`, and its first real use is counted. Check (1) first: if the
series is absent, the metric pipeline is broken and "no data" must not be
taken as "no use". `or vector(0)` then turns a window with no samples into an
explicit `0` rather than an empty result.

| Field | Replacement | Deprecated | Remove after | Before removing |
|---|---|---|---|---|
| `Participant.schemeNumber` | `memberNumber` | 2026-10-16 | 2026-12-31 | (1) `absent(graphql_deprecated_field_usage_total{field="Participant.schemeNumber"})` returns nothing, (2) `sum(increase(graphql_deprecated_field_usage_total{field="Participant.schemeNumber"}[30d])) or vector(0)` is `0`; then a pull request labelled `breaking-change-approved` that deletes the field and its row here |

## Ids (decision, 2026-10-16)

`Node.id` is each record's own id: `P1`, `PL1`, a UUIDv7, not a type-prefixed
or base64 global id. **This is a one-way door:** changing the format later
would change `Node.id` for every client that stored one, which is a breaking
change under the policy above. Kept because clients already pass these ids to
`participant(id:)`, `plan(id:)` and the mutations, and `node(id:)` must accept
the same strings. It relies on ids being unique across types: generated ids are
UUIDv7 (unique by construction) and the demo data uses distinct prefixes,
guarded by a test on each store's seed (memory and Postgres). The price:
`node` and `nodes` look an id up in each type in a fixed order, one batch read
per type per call (at most five reads, however many ids). If a new type ever
takes ids from outside (an import, a partner system) that could collide, give
that type a prefixed global id for its new records; existing ids stay as they
are.

## Unreleased

- CI: breaking-change gate for the schema (`.github/workflows/graphql-schema.yaml`).
- Added `interface Node` (Participant, Plan, Provider, Claim, SupportRequest) with `node(id:)` and `nodes(ids:)` (at most 50 ids). They apply the same authorization as the typed queries: an id the caller may not read is `null`, like a missing one.
- Added `interface Party { id displayName }` on `Participant` and `Provider` (new field `displayName` on both).
- Added `search(term:, first:)` returning `[SearchResult!]!`, `union SearchResult = Participant | Provider | Claim`.
- Added `filter: ClaimFilter` (`status`, `category`, `providerId`, `submittedAfter`, `submittedBefore`) and `orderBy: ClaimOrder` (`SUBMITTED_AT` or `AMOUNT`, `ASC` or `DESC`) to `Query.claims` and `Plan.claims`. The `status` argument stays for older clients. Cursors of the default order keep their earlier form; a cursor used with a different order is `BAD_USER_INPUT`.
- Added `interface UserError { message path }`, implemented by `ValidationError`, `BudgetExceededError`, `SelfApprovalNotAllowedError` and `IdempotencyKeyReusedError` (adds `path`, and `message` where it was missing).
- Added `SubmitClaimInput.idempotencyKey` and `type IdempotencyKeyReusedError`. **Dangerous:** `SubmitClaimPayload` gained the member `IdempotencyKeyReusedError`; it is only returned to a client that sends a key.
- Added `Participant.memberNumber`. **Deprecated** `Participant.schemeNumber` ("Use memberNumber; removal after 2026-12-31").
- Metric: `graphql.deprecated_field_usage{field="Type.field"}` counts the operations that select a deprecated field (once per operation). Every deprecated field's series starts at 0 when a pod starts.
- **Dangerous:** `node`, `nodes` and `search` are priced against the complexity limit (300): `node` costs 10 plus its selection, `nodes` 10 plus (selection + 1) per id, `search` 20 plus `first` × its selection. One `nodes(ids:)` of 50 ids with a small selection fits; five in one request, or dozens of aliased `node`/`search` fields, are `COMPLEXITY_LIMIT_EXCEEDED`.
- A malformed id (a NUL byte, invalid UTF-8, more than 128 bytes) or search term is `BAD_USER_INPUT`; one malformed id fails the whole `nodes(ids:)` call, as any malformed argument does.
