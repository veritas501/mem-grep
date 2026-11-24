---
name: test-database
description: integration tests run against the real test-db on port 5432, never a mock
metadata:
  node_type: memory
  type: feedback
  originSessionId: sess_128fb6cf-7b5f-47aa-a252-60bfda8ce855
---

Integration tests must hit the real test database; do not mock it.

**Why:** pnpm test failed with ECONNREFUSED 127.0.0.1:5432 until test-db was started, and a mock would have hidden that.
**How to apply:** start test-db before running pnpm test.
