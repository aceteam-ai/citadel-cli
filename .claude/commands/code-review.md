# Code Review

Comprehensive code review that identifies security vulnerabilities and UI/UX bugs before a PR is ready for review. Run this skill after completing feature implementation.

## Arguments

- `$ARGUMENTS` - Optional: paths to focus on (e.g., `components/workbench app/api/chat`). If empty, reviews recently changed files.

## Steps

### 1. Identify files to review

If `$ARGUMENTS` is provided, use those paths. Otherwise:

- Run `git diff --name-only origin/main` to find changed files
- Filter to relevant file types: `.ts`, `.tsx`, `.py`

### 2. Run parallel reviews

Spawn two background agents to run concurrently:

#### Agent 1: UI/UX Bug Hunter

Focus on React/TypeScript components and pages. Look for:

**React Anti-patterns:**

- State updates during render (calling setState outside useEffect/callbacks)
- Non-unique or index-based list keys that could cause state corruption
- Missing dependencies in useEffect, useMemo, useCallback
- Conditional hooks (hooks inside if statements or loops)
- Memory leaks (missing cleanup in useEffect)

**UI/UX Issues:**

- Hardcoded values that should use state (e.g., `open={true}` instead of state variable)
- Missing loading states or error handling
- Missing accessibility attributes (aria-labels on icon buttons)
- Hard navigation (`window.location.href`) instead of Next.js router
- Missing optimistic update error feedback

**Patterns to grep for:**

- `if (.*) { set[A-Z]` outside useEffect/useCallback
- `key={index}` in list mappings
- `open={true}` on Dialog/Modal components
- `window.location.href` assignments

#### Agent 2: Security Review

Focus on API routes and authentication. Look for:

**Critical Security Issues:**

- Unauthenticated endpoints (using `withErrorHandling` instead of `withAuthenticatedContext`)
- Missing authorization checks (endpoints that don't verify ownership)
- Disabled SSL verification (`rejectUnauthorized: false`)
- Unencrypted secrets in database (TODO comments about encryption)

**High Security Issues:**

- Missing input validation (no Zod schema on request body/params)
- Missing UUID validation on route parameters
- File upload without size/type limits
- Error messages exposing implementation details
- `@ts-ignore` masking type safety

**Medium Security Issues:**

- Sensitive data in logs without sanitization
- Hardcoded credentials or fallback values
- Incomplete authorization on batch operations
- Missing rate limiting on sensitive endpoints

**Patterns to grep for:**

- `withErrorHandling` in API routes (should often be `withAuthenticatedContext`)
- `rejectUnauthorized.*false`
- `@ts-ignore` in API route files
- `console.log.*password|secret|token|key` (case insensitive)

### 3. Compile findings

Wait for both agents to complete, then compile a report with:

**Summary table:**
| Category | Critical | High | Medium | Low |
|----------|----------|------|--------|-----|
| Security | X | Y | Z | W |
| UI/UX | X | Y | Z | W |

**Critical Issues (fix before merge):**

- List each critical issue with file path, line number, and remediation

**High Priority Issues:**

- List each high priority issue with file path and suggested fix

**Medium/Low Issues:**

- Brief list for follow-up

### 4. Create GitHub issues (optional)

If there are more than 3 issues found:

- Suggest creating a GitHub issue for tracking remaining fixes
- Provide issue body template with checklist

## Notes

- This skill is designed to catch issues that automated linters miss
- Focus on patterns that cause runtime bugs, not style issues
- Security issues should block PR merge; UI bugs can be follow-up
