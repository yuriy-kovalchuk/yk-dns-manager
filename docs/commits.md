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
feat: optionally limit DNS to routes a Gateway accepted

- --require-accepted-routes (chart value requireAcceptedRoutes), off by default
- an unaccepted route takes no finalizer, and records it owned are released
- the event filter also passes updates that flip acceptance
```
