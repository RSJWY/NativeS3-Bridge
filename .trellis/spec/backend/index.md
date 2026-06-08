# Backend Development Guidelines

> Best practices for backend development in this project.

---

## Overview

This directory contains guidelines for backend development. Fill in each file with your project's specific conventions.

---

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Directory Structure](./directory-structure.md) | Module organization and file layout | To fill |
| [Database Guidelines](./database-guidelines.md) | ORM patterns, queries, migrations | Filled |
| [Storage Guidelines](./storage-guidelines.md) | Native S3 object storage contracts | Filled |
| [Auth and Quota Guidelines](./auth-quota-guidelines.md) | SigV4 verification, credential cache, quota accounting | Filled |
| [Presigned Hooks Guidelines](./presigned-hooks-guidelines.md) | Query SigV4 presigned URLs and async webhook event hooks | Filled |
| [Webadmin Guidelines](./webadmin-guidelines.md) | Single-password admin API, ops endpoints, credential CRUD, dashboard data, embedded SPA serving | Filled |
| [Error Handling](./error-handling.md) | Error types, handling strategies | To fill |
| [Quality Guidelines](./quality-guidelines.md) | Code standards, forbidden patterns | To fill |
| [Logging Guidelines](./logging-guidelines.md) | Structured S3 request logging and request ID correlation | Filled |

---

## How to Fill These Guidelines

For each guideline file:

1. Document your project's **actual conventions** (not ideals)
2. Include **code examples** from your codebase
3. List **forbidden patterns** and why
4. Add **common mistakes** your team has made

The goal is to help AI assistants and new team members understand how YOUR project works.

---

**Language**: All documentation should be written in **English**.
