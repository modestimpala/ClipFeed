# Web Architecture Guide

This frontend uses a feature-first structure with shared modules.

## Folder Structure

- `src/app/`: app shell and orchestration only
- `src/features/`: feature-specific UI and logic
- `src/shared/api/`: API client and endpoint wrappers
- `src/shared/ui/`: reusable visual primitives (icons, shared components)

## Coding Ethics and Standards

- Keep files single-purpose and small.
- Keep side effects in clear boundaries (API layer, hooks, or event handlers).
- Avoid silent error swallowing unless there is an intentional UX fallback.
- Keep naming explicit and behavior-focused.
- Prefer pure utility functions for formatting and transformation logic.
- Avoid cross-feature imports unless they come from `shared/`.

## Refactor Rule of Thumb

If a file starts doing more than one of these jobs, split it:

1. Orchestrating screen flow
2. Fetching/writing API data
3. Rendering a reusable UI building block
4. Containing pure utility formatting logic
