# Commit Message Standards

This project follows the **Conventional Commits** specification, which is the standard for the Go, Kubernetes, and Cloud Native communities.

## Format
```text
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

## Types

| Type | Description |
| :--- | :--- |
| **feat** | A new feature (e.g., adding a new DNS provider) |
| **fix** | A bug fix (e.g., fixing a finalizer cleanup error) |
| **chore** | Maintenance tasks (e.g., version bumps, updating dependencies) |
| **docs** | Documentation changes only |
| **test** | Adding or correcting tests |
| **refactor** | Code change that neither fixes a bug nor adds a feature |
| **ci** | Changes to CI/CD configuration (GitHub Actions, Makefile) |

## Examples

### Feature
```text
feat(opnsense): add support for multiple Unbound host overrides
```

### Bug Fix
```text
fix(controller): resolve race condition in finalizer removal
```

### Version Bump
```text
chore(release): bump version to 0.4.0
```

### Infrastructure/CI
```text
ci: add multi-arch support to release pipeline
```

## Why it Matters
1. **Automated Changelogs**: Tools like `release-please` can automatically generate release notes based on these types.
2. **Readability**: Quickly scanning the git log reveals the intent of every change.
3. **Semantic Versioning**: `feat` implies a MINOR bump, `fix` implies a PATCH bump, and `BREAKING CHANGE` in the footer implies a MAJOR bump.
