# Release checklist

Use this checklist for the exact revision and artifacts being released. Packaging and publication are separate actions.

## Before sharing a candidate publicly

- Review dependency notices against the exact candidate using established tooling. Project code uses MIT; retain LICENSE, THIRD_PARTY_NOTICES.md, and third_party/ in distributed artifacts.
- Run an established secret scanner, such as Gitleaks, over the exact extracted source candidate. If any history will be published, scan that history too. Keep reports private and redact candidate values; never test suspected credentials against a provider.
- Inspect the archive contents for operator files, private notes, identifiers, and accidentally tracked configuration. Git export exclusions are a convenience, not secret detection.

## Verify the deployment people will use

Use the [packaging guide](release-packaging.md) and [supported onboarding guide](supported-onboarding.md) in a clean, disposable environment with synthetic credentials and mail.

- Install from the candidate artifacts and run migrations.
- Onboard a synthetic mailbox and two agents; verify alias isolation, read, send, and reply behavior.
- Restart the service and verify persistence and deduplication.
- Back up and restore the database and required encryption keys; verify an upgrade from the preceding supported version.
- Check that the documented commands, prerequisites, and provider limitations match the result.

Record the revision, commands, results, and remaining limitations in the release PR. Reuse existing test scripts where they cover these behaviors. State which checks passed and which were skipped.

## Publication

Publication needs maintainer approval after the license, selected-artifact review, and deployment checks are complete. Do not change repository visibility, publish a release or image as part of packaging.
