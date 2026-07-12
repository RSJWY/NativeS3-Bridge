# Implement: Close quota bypass vectors

1. Add atomic reserve, settle, and release database operations with concurrent tests.
2. Pass a reservation manager through server/router and write handlers.
3. Enforce declared PUT length with a quota-aware reader.
4. Reserve and settle PUT, CopyObject, and multipart completion.
5. Add configurable multipart pending-byte cap and storage tests.
6. Run package tests, race tests, then the repository-wide regression suite.
