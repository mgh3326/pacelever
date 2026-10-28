# Design

The library evaluates immutable inputs. The CLI loads configuration, gathers observations,
and renders the stable command contract. No provider credentials belong in an admission
result, catalog, diagnostic, fixture, or log.

Admission and launch resolution share catalog selection and policy checks. Explicit role
checks precede quota checks. A placed default-gate credit rung may hold a builder seat;
consult-only, escalation, and unplaced credit rungs remain worker/tester only. Credit
profiles require an explicit effort and seat.

Pace changes ordering and produces warnings. It never excludes a placed model or denies
admission. The SWE-2 free bucket precedes paid alternatives and is independent of account
credits. Manual observations are a bounded fallback when automatic measurement fails;
a known automatic cutoff remains authoritative.

Configuration and observations fall back to existing scopefuel files without modifying
them. New writes go to pacelever-owned files. Migration begins in shadow: compare gate
verdicts from both executables before changing any caller. Installing or replacing a
running tool is a separate operator action.
