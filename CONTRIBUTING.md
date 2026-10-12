# Contributing to git-hint

`git-hint` is the open client of HINT and the home of the HINT data standard. Write documentation in short, direct English sentences in the ASD-STE100 style.

## 1. Independent implementation (clean-room rule)

HINT does not copy code that it has no right to use.

1. **Open libraries first.** Use an open-source library with a compatible license when it solves the task. Check its license and its transitive dependencies before you add it. Record the library, version and license in the README of the component that uses it.
2. **Restricted or closed projects.** A restricted license (for example non-commercial, source-available, competing-use, or no license) excludes the component. It does not exclude a useful approach. For such projects:
   - Use only public descriptions, papers, interfaces, documentation and observed behavior.
   - Do not read, copy, translate or paraphrase their source code.
   - Separate the approach from the implementation, and write your own design and code.
3. **Validate with own tests.** Validate the implementation with synthetic data or recorded, scrubbed fixtures.
4. **Third-party tools stay unmodified.** Do not fork or patch a third-party product to fill a HINT gap.
5. **Notices.** Keep the copyright and license notices that a reused component requires. Pin each reused component to a version or commit with a confirmed license file.

## 2. Data and privacy

- Never commit raw captures, credentials, account IDs, personal paths or real customer data.
- Test fixtures are synthetic or scrubbed. Canary values are fake.
- The local store `.hint/` is ignored by Git. Never commit it.

## 3. Tests

Tests run without an AI agent or a model: unit tests with synthetic data, recorded and scrubbed fixtures, mock events and deterministic replay. Run the conformance suite as described in [`tests/conformance/README.md`](tests/conformance/README.md).

## 4. Scope

This repository holds the client and the public contract: the data format, the API and the client configuration.
