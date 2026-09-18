- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD workflow must not auto-commit without explicit user authorization.
  evidence: Generated step-05 and oneshot instructions require a dirty-tree commit, conflicting with repository policy; this run obeys the higher-priority prohibition.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: Installed BMAD renderer tests need self-contained fixtures and inclusion in a quality gate.
  evidence: Standalone collection fails on absent assets/skills and `make quality` never selects `_bmad/scripts/tests`.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD memlog needs symlink-safe unique temporary files and serialized append updates.
  evidence: The fixed `.tmp` name follows symlinks; concurrent append was reproduced losing entries and raising during replace.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD memlog must validate entry labels and frontmatter keys.
  evidence: Multiline or reserved key/type/by inputs can corrupt the single-line chronology or frontmatter semantics.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD setup output paths and renderer publication paths need descendant and symlink containment checks.
  evidence: Unsafe `output_folder` and symlinked render/output components can write outside the intended project tree.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD updater must validate source manifest module identity.
  evidence: Source parsing accepts any non-empty version and can report state from an unrelated module manifest.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD module-script repair must preserve safe mode bits and remove no-longer-declared scripts.
  evidence: Script trees store bytes only, repair writes default modes, and seeded stale scripts survive a newer manifest.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD keyed-array merges need strict identity validation.
  evidence: Missing override identities switch to append semantics and duplicate base identities remain conflicting.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD setup and doctor replacement behavior needs preservation and rollback tests.
  evidence: No test exercises custom/unmanaged state preservation or restoration after a forced replacement failure.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD update-state classification needs an offline SemVer/source matrix test.
  evidence: No existing test covers current, upgrade, ahead, prerelease, unordered, unreachable or disagreement states.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD config resolvers need normalized TOML scalars and concise invalid-UTF8 failures.
  evidence: Date/time values are not JSON serializable and invalid UTF-8 escapes the ConfigError boundary.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD setup needs strict validation for answers, required directory types and bounded local source reads.
  evidence: Non-string answered values, files/broken symlinks at required directories, and unbounded `file:` reads are accepted or mishandled.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD review workflow needs fresh approval after intent edits and a second review after patches.
  evidence: Current steps continue after externally changed specs and finalize patched code without rerunning independent layers.
- source_spec: `spec-hl-263-r01-r14-tail-evidence-registry.md`
  summary: BMAD loopback revert needs exact path ownership and preservation rules.
  evidence: The generic “revert code changes” instruction can remove spec or foreign work in a mixed tree.
