# Commit Messages

Format: `type: short description` (72 chars max on first line)

| Type | Use when |
|------|----------|
| `feat` | adding a new feature |
| `fix` | fixing a bug |
| `refactor` | restructuring code without behaviour change |
| `test` | adding or updating tests |
| `docs` | documentation only |
| `build` | Makefile, Dockerfile, dependencies |
| `ci` | GitHub Actions, CI config |
| `chore` | maintenance (version bumps, generated files) |
| `perf` | performance improvement |
| `style` | formatting, whitespace, no logic change |

Breaking changes: append `!` — `feat!: ...` — and explain in the body.

Body is free-form. Bullet points work well for multi-part commits:

```
feat: add config drift detection

- periodic comparison of running vs desired machine config via Talos API
- opt-out per node via spec.driftDetection=false (default: true)
- offline nodes skipped silently, no status change
```
