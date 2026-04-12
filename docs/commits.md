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
| **perf** | A performance improvement |
| **build** | Changes to build system or external dependencies |
| **chore** | Maintenance tasks (e.g., version bumps, updating dependencies) |
| **docs** | Documentation changes only |
| **style** | Formatting, whitespace (no logic changes) |
| **test** | Adding or correcting tests |
| **refactor** | Code change that neither fixes a bug nor adds a feature |
| **ci** | Changes to CI/CD configuration (GitHub Actions, Makefile) |
| **revert** | Reverts a previous commit |

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

### Performance
```text
perf(dns): cache DNS queries to reduce API calls
```

### Build
```text
build: upgrade Go to 1.23
```

### Documentation
```text
docs: update README with new provider setup steps
```

### Style
```text
style: format code with gofmt
```

### Test
```text
test(controller): add unit tests for finalizer logic
```

### Refactor
```text
refactor(provider): extract common HTTP client logic
```

### Revert
```text
revert: fix(controller): resolve race condition
```

## Why it Matters
1. **Automated Changelogs**: Tools like `release-please` can automatically generate release notes based on these types.
2. **Readability**: Quickly scanning the git log reveals the intent of every change.
3. **Semantic Versioning**: `feat` implies a MINOR bump, `fix` implies a PATCH bump, and `BREAKING CHANGE` in the footer implies a MAJOR bump.
